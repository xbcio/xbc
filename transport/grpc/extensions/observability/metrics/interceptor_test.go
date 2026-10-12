package metrics

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testFullMethod = "/xbc.test.Metrics/Echo"

func TestUnaryInterceptionRecordsBoundedSeries(t *testing.T) {
	p, core := newTestPlugin(t)

	info := &grpc.UnaryServerInfo{FullMethod: testFullMethod}
	handler := func(context.Context, any) (any, error) { return "resp", nil }
	if _, err := p.UnaryInterceptor()(context.Background(), "req", info, handler); err != nil {
		t.Fatal(err)
	}
	failing := func(context.Context, any) (any, error) { return nil, status.Error(codes.NotFound, "missing") }
	if _, err := p.UnaryInterceptor()(context.Background(), "req", info, failing); status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v", err)
	}

	families := gather(t, core)
	for code, want := range map[string]float64{"OK": 1, "NotFound": 1} {
		labels := map[string]string{"service": "xbc.test.Metrics", "method": "Echo", "code": code}
		if value, ok := seriesValue(families, "xbc_grpc_requests_total", labels); !ok || value != want {
			t.Errorf("xbc_grpc_requests_total%v = %v (found=%t), want %v", labels, value, ok, want)
		}
		if count, _, ok := histogramSample(families, "xbc_grpc_request_duration_seconds", labels); !ok || count != 1 {
			t.Errorf("xbc_grpc_request_duration_seconds%v count = %d (found=%t), want 1", labels, count, ok)
		}
	}
	inflightLabels := map[string]string{"service": "xbc.test.Metrics", "method": "Echo"}
	if value, ok := seriesValue(families, "xbc_grpc_requests_in_flight", inflightLabels); !ok || value != 0 {
		t.Fatalf("xbc_grpc_requests_in_flight%v = %v (found=%t), want 0 after both calls", inflightLabels, value, ok)
	}
}

func TestInFlightGaugeIsVisibleWhileTheCallRuns(t *testing.T) {
	p, core := newTestPlugin(t)

	_, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(context.Context, any) (any, error) {
			value, ok := seriesValue(gather(t, core), "xbc_grpc_requests_in_flight",
				map[string]string{"service": "xbc.test.Metrics", "method": "Echo"})
			if !ok || value != 1 {
				t.Errorf("in-flight during the call = %v (found=%t), want 1", value, ok)
			}
			return "resp", nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPanicRecordsInternalAndStillReleasesTheInFlightGauge(t *testing.T) {
	p, core := newTestPlugin(t)

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		_, _ = p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
			func(context.Context, any) (any, error) { panic("boom") })
	}()

	families := gather(t, core)
	labels := map[string]string{"service": "xbc.test.Metrics", "method": "Echo", "code": "Internal"}
	if value, ok := seriesValue(families, "xbc_grpc_requests_total", labels); !ok || value != 1 {
		t.Fatalf("xbc_grpc_requests_total%v = %v (found=%t), want 1", labels, value, ok)
	}
	if value, ok := seriesValue(families, "xbc_grpc_requests_in_flight",
		map[string]string{"service": "xbc.test.Metrics", "method": "Echo"}); !ok || value != 0 {
		t.Fatalf("in-flight after the panic = %v (found=%t), want 0", value, ok)
	}
}

// fakeServerStream is the minimal ServerStream the stream interceptor touches.
type fakeServerStream struct {
	ctx context.Context
}

func (s *fakeServerStream) SetHeader(metadata.MD) error { return nil }

func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }

func (s *fakeServerStream) SetTrailer(metadata.MD) {}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

func (*fakeServerStream) SendMsg(any) error { return nil }

func (*fakeServerStream) RecvMsg(any) error { return nil }

func TestStreamInterceptionRecordsOneCallForTheWholeStream(t *testing.T) {
	p, core := newTestPlugin(t)

	stream := &fakeServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: testFullMethod}
	err := p.StreamInterceptor()(nil, stream, info,
		func(any, grpc.ServerStream) error { return status.Error(codes.PermissionDenied, "nope") })
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v", err)
	}

	families := gather(t, core)
	labels := map[string]string{"service": "xbc.test.Metrics", "method": "Echo", "code": "PermissionDenied"}
	if value, ok := seriesValue(families, "xbc_grpc_requests_total", labels); !ok || value != 1 {
		t.Fatalf("xbc_grpc_requests_total%v = %v (found=%t), want 1", labels, value, ok)
	}
	if value, ok := seriesValue(families, "xbc_grpc_requests_in_flight",
		map[string]string{"service": "xbc.test.Metrics", "method": "Echo"}); !ok || value != 0 {
		t.Fatalf("in-flight after the stream = %v (found=%t), want 0", value, ok)
	}
}

func TestStreamPanicRecordsInternal(t *testing.T) {
	p, core := newTestPlugin(t)

	stream := &fakeServerStream{ctx: context.Background()}
	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		_ = p.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: testFullMethod},
			func(any, grpc.ServerStream) error { panic("boom") })
	}()

	labels := map[string]string{"service": "xbc.test.Metrics", "method": "Echo", "code": "Internal"}
	if value, ok := seriesValue(gather(t, core), "xbc_grpc_requests_total", labels); !ok || value != 1 {
		t.Fatalf("xbc_grpc_requests_total%v = %v (found=%t), want 1", labels, value, ok)
	}
}

// TestZeroPluginDegradesToPassThrough pins the nil-safety the chains rely on:
// an instrumentation value without a registry never fails a call.
func TestZeroPluginDegradesToPassThrough(t *testing.T) {
	var p Plugin
	if _, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(context.Context, any) (any, error) { return "resp", nil }); err != nil {
		t.Fatal(err)
	}
	stream := &fakeServerStream{ctx: context.Background()}
	if err := p.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: testFullMethod},
		func(any, grpc.ServerStream) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSplitFullMethodKeepsLabelsBounded(t *testing.T) {
	cases := []struct {
		fullMethod string
		service    string
		method     string
	}{
		{"/xbc.test.Metrics/Echo", "xbc.test.Metrics", "Echo"},
		{"xbc.test.Metrics/Echo", "xbc.test.Metrics", "Echo"},
		{"/xbc.test.Metrics/Sub/Echo", "xbc.test.Metrics/Sub", "Echo"},
		{"/xbc.test.Metrics/", "unknown", "unknown"},
		{"/", "unknown", "unknown"},
		{"", "unknown", "unknown"},
		{"Echo", "Echo", "unknown"},
	}
	for _, c := range cases {
		service, method := splitFullMethod(c.fullMethod)
		if service != c.service || method != c.method {
			t.Errorf("splitFullMethod(%q) = (%q, %q), want (%q, %q)", c.fullMethod, service, method, c.service, c.method)
		}
	}
}
