package metrics

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// wireService is the handler interface of a hand-written test service whose
// descriptor has the shape protoc-gen-go-grpc generates.
type wireService interface {
	Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Repeat(*wrapperspb.StringValue, grpc.ServerStream) error
}

const wireServiceName = "xbc.test.MetricsWire"

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

type wireImpl struct {
	panicOn string
}

func (w *wireImpl) Echo(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	if w.panicOn != "" && in.GetValue() == w.panicOn {
		panic("wire panic")
	}
	return wrapperspb.String("echo:" + in.GetValue()), nil
}

func (w *wireImpl) Repeat(in *wrapperspb.StringValue, stream grpc.ServerStream) error {
	for range 2 {
		if err := stream.SendMsg(wrapperspb.String("echo:" + in.GetValue())); err != nil {
			return err
		}
	}
	return nil
}

// recoverUnary mirrors the Server's panic boundary, which sits outside every
// contributed interceptor and answers a re-panicked call with Internal.
func recoverUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = nil, status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

func startWireServing(t *testing.T, impl wireService, p *Plugin) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoverUnary, p.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(p.StreamInterceptor()),
	)
	server.RegisterService(wireDesc(), impl)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///"+listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestWireCallsRecordTheDocumentedSeries binds the interceptors to a real
// server: one unary call, one panicking unary call, and one streaming call
// must land as three counted series with matching histogram samples.
func TestWireCallsRecordTheDocumentedSeries(t *testing.T) {
	p, core := newTestPlugin(t)
	conn := startWireServing(t, &wireImpl{panicOn: "boom"}, p)

	response := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), wireEchoMethod, wrapperspb.String("hi"), response); err != nil {
		t.Fatal(err)
	}
	if response.GetValue() != "echo:hi" {
		t.Fatalf("response = %q", response.GetValue())
	}
	err := conn.Invoke(context.Background(), wireEchoMethod, wrapperspb.String("boom"), new(wrapperspb.StringValue))
	if status.Code(err) != codes.Internal {
		t.Fatalf("panic call error = %v", err)
	}

	stream, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}, wireRepeatMethod)
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
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
	}

	families := gather(t, core)
	service := wireServiceName
	want := []struct {
		method string
		code   string
	}{
		{"Echo", "OK"},
		{"Echo", "Internal"},
		{"Repeat", "OK"},
	}
	for _, expected := range want {
		labels := map[string]string{"service": service, "method": expected.method, "code": expected.code}
		if value, ok := seriesValue(families, "xbc_grpc_requests_total", labels); !ok || value != 1 {
			t.Errorf("xbc_grpc_requests_total%v = %v (found=%t), want 1", labels, value, ok)
		}
		if count, _, ok := histogramSample(families, "xbc_grpc_request_duration_seconds", labels); !ok || count != 1 {
			t.Errorf("xbc_grpc_request_duration_seconds%v count = %d (found=%t), want 1", labels, count, ok)
		}
	}
	for _, method := range []string{"Echo", "Repeat"} {
		labels := map[string]string{"service": service, "method": method}
		if value, ok := seriesValue(families, "xbc_grpc_requests_in_flight", labels); !ok || value != 0 {
			t.Errorf("xbc_grpc_requests_in_flight%v = %v (found=%t), want 0", labels, value, ok)
		}
	}
}
