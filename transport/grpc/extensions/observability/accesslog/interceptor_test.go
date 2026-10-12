package accesslog

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	corelog "github.com/xbcio/xbc/log"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

const testFullMethod = "/xbc.test.AccessLog/Echo"

type captured struct {
	level  string
	msg    string
	fields map[string]any
}

type captureLogger struct {
	mu      sync.Mutex
	entries []captured
}

func (l *captureLogger) add(level, msg string, kv ...any) {
	fields := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		fields[fmt.Sprint(kv[i])] = kv[i+1]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, captured{level: level, msg: msg, fields: fields})
}
func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) corelog.Logger  { return l }
func (*captureLogger) Enabled(corelog.Level) bool    { return true }

func (l *captureLogger) only(t *testing.T) captured {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) != 1 {
		t.Fatalf("records = %d, want 1: %#v", len(l.entries), l.entries)
	}
	return l.entries[0]
}

func newTestPlugin(t *testing.T, cfg Config) (*Plugin, *captureLogger) {
	t.Helper()
	logger := &captureLogger{}
	value, err := newPlugin(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	return value, logger
}

func peerContext() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 40123}})
}

func callUnary(p *Plugin, ctx context.Context, handler grpc.UnaryHandler) error {
	_, err := p.UnaryInterceptor()(ctx, "req", &grpc.UnaryServerInfo{FullMethod: testFullMethod}, handler)
	return err
}

func TestOrderIsTheObservationPhaseWithoutEdges(t *testing.T) {
	order := New().Order()
	if order.Phase != grpctransport.PhaseObserve || len(order.After) != 0 || len(order.Before) != 0 {
		t.Fatalf("Order() = %#v, want the observation phase and no edges", order)
	}
}

func TestUnaryRecordCarriesTheDocumentedFields(t *testing.T) {
	p, logger := newTestPlugin(t, DefaultConfig())
	if err := callUnary(p, peerContext(), func(context.Context, any) (any, error) { return "resp", nil }); err != nil {
		t.Fatal(err)
	}

	record := logger.only(t)
	if record.level != "info" || record.msg != "grpc call completed" {
		t.Fatalf("record = %s %q, want info \"grpc call completed\"", record.level, record.msg)
	}
	want := map[string]any{
		"service":  "xbc.test.AccessLog",
		"method":   "Echo",
		"kind":     "unary",
		"code":     "OK",
		"peer":     "192.0.2.7",
		"panicked": false,
	}
	for key, value := range want {
		if record.fields[key] != value {
			t.Errorf("field %s = %#v, want %#v", key, record.fields[key], value)
		}
	}
	if _, ok := record.fields["latency"].(time.Duration); !ok {
		t.Errorf("latency = %#v, want a time.Duration", record.fields["latency"])
	}
	if len(record.fields) != len(want)+1 {
		t.Errorf("fields = %v, want exactly the documented set", record.fields)
	}
}

func TestLevelFollowsTheCodeAndTheSlowThreshold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		level string
		code  string
	}{
		{"client error", status.Error(codes.NotFound, "missing"), "info", "NotFound"},
		{"refusal", status.Error(codes.PermissionDenied, "forbidden"), "info", "PermissionDenied"},
		{"server fault", status.Error(codes.Unavailable, "down"), "error", "Unavailable"},
		{"internal", status.Error(codes.Internal, "internal error"), "error", "Internal"},
		{"deadline", status.Error(codes.DeadlineExceeded, "late"), "error", "DeadlineExceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, logger := newTestPlugin(t, DefaultConfig())
			_ = callUnary(p, context.Background(), func(context.Context, any) (any, error) { return nil, tc.err })
			record := logger.only(t)
			if record.level != tc.level || record.fields["code"] != tc.code {
				t.Fatalf("record = %s code=%v, want %s code=%s", record.level, record.fields["code"], tc.level, tc.code)
			}
		})
	}

	t.Run("slow", func(t *testing.T) {
		p, logger := newTestPlugin(t, Config{SlowCall: time.Millisecond})
		_ = callUnary(p, context.Background(), func(context.Context, any) (any, error) {
			time.Sleep(5 * time.Millisecond)
			return "resp", nil
		})
		record := logger.only(t)
		if record.level != "warn" || record.msg != "grpc call completed slowly" {
			t.Fatalf("record = %s %q, want the slow warning", record.level, record.msg)
		}
	})

	t.Run("slow server fault stays an error", func(t *testing.T) {
		p, logger := newTestPlugin(t, Config{SlowCall: time.Nanosecond})
		_ = callUnary(p, context.Background(), func(context.Context, any) (any, error) {
			time.Sleep(time.Millisecond)
			return nil, status.Error(codes.Internal, "internal error")
		})
		if record := logger.only(t); record.level != "error" {
			t.Fatalf("level = %s, want error", record.level)
		}
	})
}

func TestPanicIsRecordedAsInternalAndPropagated(t *testing.T) {
	p, logger := newTestPlugin(t, DefaultConfig())
	func() {
		defer func() {
			if recovered := recover(); recovered != "secret-bearing panic" {
				t.Fatalf("recovered = %#v, want the original panic to continue", recovered)
			}
		}()
		_ = callUnary(p, context.Background(), func(context.Context, any) (any, error) { panic("secret-bearing panic") })
	}()

	record := logger.only(t)
	if record.level != "error" || record.fields["code"] != "Internal" || record.fields["panicked"] != true {
		t.Fatalf("record = %s %v, want an Internal panicked error record", record.level, record.fields)
	}
	assertNoLeak(t, record, "secret-bearing")
}

// TestRecordsNeverCarryMetadataMessagesOrErrorText pins the minimization
// promise: credentials in metadata, the request value, and error and status
// text all stay out of the record.
func TestRecordsNeverCarryMetadataMessagesOrErrorText(t *testing.T) {
	p, logger := newTestPlugin(t, DefaultConfig())
	ctx := metadata.NewIncomingContext(peerContext(), metadata.Pairs(
		"authorization", "Bearer token-value-123",
		"x-api-key", "key-value-456",
		"x-forwarded-for", "203.0.113.9",
	))
	_, err := p.UnaryInterceptor()(ctx, "request-payload-789", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(context.Context, any) (any, error) {
			return nil, status.Error(codes.InvalidArgument, "status-text phone 13800000000")
		})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v", err)
	}

	record := logger.only(t)
	assertNoLeak(t, record, "token-value-123", "key-value-456", "203.0.113.9", "request-payload-789", "status-text", "13800000000")
	if record.fields["peer"] != "192.0.2.7" {
		t.Fatalf("peer = %v, want the transport address, not forwarded metadata", record.fields["peer"])
	}
}

func assertNoLeak(t *testing.T, record captured, needles ...string) {
	t.Helper()
	rendered := fmt.Sprint(record.msg, record.fields)
	for _, needle := range needles {
		if strings.Contains(rendered, needle) {
			t.Errorf("record %q leaks %q", rendered, needle)
		}
	}
}

func TestSkipMethodsSuppressTheRecordButNotTheCall(t *testing.T) {
	p, logger := newTestPlugin(t, Config{SkipMethods: []string{"/xbc.test.AccessLog/*", "/other.Service/Exact"}})
	called := false
	if err := callUnary(p, context.Background(), func(context.Context, any) (any, error) {
		called = true
		return "resp", nil
	}); err != nil || !called {
		t.Fatalf("skipped call: called=%t err=%v", called, err)
	}
	_, _ = p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: "/other.Service/ExactNot"},
		func(context.Context, any) (any, error) { return "resp", nil })

	record := logger.only(t)
	if record.fields["method"] != "ExactNot" {
		t.Fatalf("recorded %v, want only the call no rule matched", record.fields)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeStream) Context() context.Context { return s.ctx }

func TestStreamIsOneRecordWithItsKind(t *testing.T) {
	for _, tc := range []struct {
		client, server bool
		kind           string
	}{
		{false, true, "server_stream"},
		{true, false, "client_stream"},
		{true, true, "bidi_stream"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			p, logger := newTestPlugin(t, DefaultConfig())
			info := &grpc.StreamServerInfo{FullMethod: testFullMethod, IsClientStream: tc.client, IsServerStream: tc.server}
			err := p.StreamInterceptor()(nil, fakeStream{ctx: peerContext()}, info, func(any, grpc.ServerStream) error {
				return status.Error(codes.Aborted, "aborted")
			})
			if status.Code(err) != codes.Aborted {
				t.Fatalf("err = %v", err)
			}
			record := logger.only(t)
			if record.fields["kind"] != tc.kind || record.fields["code"] != "Aborted" || record.fields["peer"] != "192.0.2.7" {
				t.Fatalf("record = %v", record.fields)
			}
		})
	}
}

func TestStreamPanicIsRecordedAsInternal(t *testing.T) {
	p, logger := newTestPlugin(t, DefaultConfig())
	func() {
		defer func() { _ = recover() }()
		_ = p.StreamInterceptor()(nil, fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: testFullMethod},
			func(any, grpc.ServerStream) error { panic("boom") })
	}()
	if record := logger.only(t); record.fields["code"] != "Internal" || record.fields["panicked"] != true {
		t.Fatalf("record = %v", record.fields)
	}
}

func TestPeerAndMethodDegradeToPlaceholders(t *testing.T) {
	if got := peerIP(context.Background()); got != "unknown" {
		t.Errorf("peerIP(no peer) = %q", got)
	}
	unix := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.UnixAddr{Name: "/tmp/sock", Net: "unix"}})
	if got := peerIP(unix); got != "unknown" {
		t.Errorf("peerIP(unix) = %q", got)
	}
	for _, input := range []string{"", "/", "noslash", "/svc/", "//m"} {
		if service, method := splitFullMethod(input); service != "unknown" || method != "unknown" {
			t.Errorf("splitFullMethod(%q) = %q, %q", input, service, method)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{SlowCall: -time.Second},
		{SkipMethods: []string{""}},
		{SkipMethods: []string{"pkg.Service/Method"}},
		{SkipMethods: []string{"/pkg.*/Method"}},
		{SkipMethods: []string{"/pkg.Service/Me thod"}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%#v) = nil, want an error", cfg)
		} else if !strings.HasPrefix(err.Error(), "grpc-accesslog: ") {
			t.Errorf("Validate(%#v) = %v, want the plugin prefix", cfg, err)
		}
	}
	if err := (Config{SkipMethods: []string{" /pkg.Service/* "}, SlowCall: time.Second}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := newPlugin(Config{SlowCall: -1}, nil); err == nil {
		t.Fatal("newPlugin accepted an invalid config")
	}
	if _, err := prepareConfig(Config{SlowCall: -1}); err == nil {
		t.Fatal("prepareConfig accepted an invalid config")
	}
}
