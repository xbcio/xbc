package accesslog

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/config"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// wireService is the handler interface of a hand-written test service whose
// descriptor has the shape protoc-gen-go-grpc generates.
type wireService interface {
	Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Repeat(*wrapperspb.StringValue, grpc.ServerStream) error
}

const wireServiceName = "xbc.test.AccessLogWire"

const (
	wireEchoMethod   = "/" + wireServiceName + "/Echo"
	wireRepeatMethod = "/" + wireServiceName + "/Repeat"
)

func wireDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: wireServiceName,
		HandlerType: (*wireService)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(wireService).Echo(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: wireEchoMethod}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return srv.(wireService).Echo(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Repeat",
			ServerStreams: true,
			Handler: func(srv any, stream grpc.ServerStream) error {
				in := new(wrapperspb.StringValue)
				if err := stream.RecvMsg(in); err != nil {
					return err
				}
				return srv.(wireService).Repeat(in, stream)
			},
		}},
	}
}

type wireImpl struct{}

func (wireImpl) Echo(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	switch in.GetValue() {
	case "raw":
		return nil, errors.New("database password=hunter2 at /var/lib/db")
	case "boom":
		panic("wire panic")
	}
	return wrapperspb.String("echo:" + in.GetValue()), nil
}

func (wireImpl) Repeat(in *wrapperspb.StringValue, stream grpc.ServerStream) error {
	return stream.SendMsg(wrapperspb.String("echo:" + in.GetValue()))
}

type wirePlugin struct{}

func (wirePlugin) RegisterServices(r *grpctransport.Registrar) error {
	r.RegisterService(wireDesc(), wireImpl{})
	r.Methods(wireServiceName).Auth(grpctransport.Public())
	return nil
}

var wireBundle = plugin.BundleOf(plugin.Define(
	"test-accesslog-wire",
	func(plugin.BuildContext) (*wirePlugin, error) { return new(wirePlugin), nil },
	plugin.Options[*wirePlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.ServiceRegistrar](func(value *wirePlugin) grpctransport.ServiceRegistrar { return value }),
		),
	},
))

// wireHost is the smallest faithful RuntimeHost a full-transport test needs.
// Its logger is the capture logger; the plan is built with the same one, so
// the access log's BuildContext logger is the one the test reads.
type wireHost struct {
	logger    *captureLogger
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*wireHost)(nil)

func (h *wireHost) ExecutionContext() context.Context                               { return h.execution }
func (h *wireHost) Logger() corelog.Logger                                          { return h.logger }
func (*wireHost) ProcessInstance() string                                           { return "accesslog-wire-test-process" }
func (h *wireHost) TrafficGate() <-chan struct{}                                    { return h.gate }
func (*wireHost) Admission(plugin.Identity) plugin.Admission                        { return nil }
func (*wireHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*wireHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (h *wireHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.execution)
	}()
	return true
}

func (h *wireHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

// bootWire composes the real gRPC server, this plugin, and the test service
// through the real assembly, and returns a connected client.
func bootWire(t *testing.T) (*grpc.ClientConn, *captureLogger) {
	t.Helper()
	execution, cancel := context.WithCancel(context.Background())
	host := &wireHost{logger: &captureLogger{}, execution: execution, cancel: cancel, gate: make(chan struct{})}
	t.Cleanup(host.shutdown)

	environment, err := config.NewEnvironment(map[string]any{
		"grpc":    map[string]any{"addr": "127.0.0.1:0"},
		"plugins": map[string]any{Key.String(): map[string]any{"enabled": true}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{grpctransport.Bundle(), Bundle(), wireBundle},
		Env:     environment,
		Logger:  host.logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ corelog.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: grpctransport.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the gRPC server was not constructed")
	}
	server := instance.Primary().(*grpctransport.Server)
	serverContext := plugin.NewRuntimeContext(host, plugin.Identity{Plugin: grpctransport.Key, Instance: plugin.DefaultInstance})
	if err := server.Start(serverContext); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopContext, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		_ = server.Stop(stopContext)
	})
	if err := server.OpenTraffic(serverContext); err != nil {
		t.Fatal(err)
	}
	host.gateOnce.Do(func() { close(host.gate) })

	conn, err := grpc.NewClient(server.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, host.logger
}

// callRecords returns only the access log's records; the server writes its
// own lifecycle and error lines through the same host logger.
func (l *captureLogger) callRecords() []captured {
	l.mu.Lock()
	defer l.mu.Unlock()
	var records []captured
	for _, entry := range l.entries {
		if entry.msg == "grpc call completed" || entry.msg == "grpc call completed slowly" {
			records = append(records, entry)
		}
	}
	return records
}

// TestWireRecordsTheCodeTheClientReceives is the placement check: on a real
// server the access log sits outside the status boundary, so a handler's raw
// error is recorded as the mapped codes.Internal the client sees, and a panic
// is recorded once as Internal while the boundary still answers the call.
func TestWireRecordsTheCodeTheClientReceives(t *testing.T) {
	conn, logger := bootWire(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, wireEchoMethod, wrapperspb.String("hi"), out, grpc.WaitForReady(true)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Invoke(ctx, wireEchoMethod, wrapperspb.String("raw"), out); status.Code(err) != codes.Internal {
		t.Fatalf("raw error call = %v, want Internal", err)
	}
	if err := conn.Invoke(ctx, wireEchoMethod, wrapperspb.String("boom"), out); status.Code(err) != codes.Internal {
		t.Fatalf("panic call = %v, want Internal", err)
	}
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}, wireRepeatMethod)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String("hi")); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	for {
		if err := stream.RecvMsg(new(wrapperspb.StringValue)); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
	}

	// The stream's record is written when its handler returns, which can be
	// just after the client has read EOF.
	deadline := time.Now().Add(5 * time.Second)
	for len(logger.callRecords()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	records := logger.callRecords()
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4: %#v", len(records), records)
	}
	want := []struct {
		method, kind, code, level string
		panicked                  bool
	}{
		{"Echo", "unary", "OK", "info", false},
		{"Echo", "unary", "Internal", "error", false},
		{"Echo", "unary", "Internal", "error", true},
		{"Repeat", "server_stream", "OK", "info", false},
	}
	for i, w := range want {
		r := records[i]
		if r.fields["service"] != wireServiceName || r.fields["method"] != w.method || r.fields["kind"] != w.kind ||
			r.fields["code"] != w.code || r.level != w.level || r.fields["panicked"] != w.panicked || r.fields["peer"] != "127.0.0.1" {
			t.Errorf("record %d = %s %v, want %+v", i, r.level, r.fields, w)
		}
		assertNoLeak(t, r, "hunter2", "/var/lib/db", "wire panic")
	}
}
