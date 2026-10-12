package tracing

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	otelcodes "go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// wireService is the handler interface of a hand-written test service whose
// descriptor has the shape protoc-gen-go-grpc generates.
type wireService interface {
	Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Repeat(*wrapperspb.StringValue, grpc.ServerStream) error
}

const wireServiceName = "xbc.test.Tracing"

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
	for range 3 {
		if err := stream.SendMsg(wrapperspb.String("echo:" + in.GetValue())); err != nil {
			return err
		}
	}
	return nil
}

// recoverUnary mirrors the Server's panic boundary, which sits outside every
// contributed interceptor: it is what turns the re-panicked call into a wire
// status, so the span's Internal record must match its answer.
func recoverUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = nil, status.Error(codes.Internal, internalStatusMessage)
		}
	}()
	return handler(ctx, req)
}

// startWireServing runs a real gRPC server with the interceptor's chains in
// front of the test service.
func startWireServing(t *testing.T, impl wireService, i *Interceptor) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoverUnary, i.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(i.StreamInterceptor()),
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

func TestUnaryCallCarriesTheInterceptedSpanOnTheWire(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	conn := startWireServing(t, &wireImpl{}, i)

	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", incomingTraceparent)
	var header metadata.MD
	response := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, wireEchoMethod, wrapperspb.String("hello"), response, grpc.Header(&header)); err != nil {
		t.Fatal(err)
	}
	if response.GetValue() != "echo:hello" {
		t.Fatalf("response = %q", response.GetValue())
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != wireEchoMethod {
		t.Fatalf("span name = %q", span.Name)
	}
	wantTraceID, err := trace.TraceIDFromHex(incomingTraceID)
	if err != nil {
		t.Fatal(err)
	}
	wantParentID, err := trace.SpanIDFromHex(incomingSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if span.SpanContext.TraceID() != wantTraceID || span.Parent.SpanID() != wantParentID || !span.Parent.IsRemote() {
		t.Fatalf("span context = %v parent = %v", span.SpanContext, span.Parent)
	}
	if got, ok := spanAttribute(span.Attributes, semconv.RPCResponseStatusCodeKey); !ok || got.AsString() != codes.OK.String() {
		t.Fatalf("rpc.response.status_code = %v, present=%v", got, ok)
	}
	if got := header.Get(strings.ToLower(defaultTraceIDHeader)); len(got) != 1 || got[0] != wantTraceID.String() {
		t.Fatalf("response metadata %s = %v, want %s", defaultTraceIDHeader, got, wantTraceID)
	}
	if got := header.Get(strings.ToLower(defaultSpanIDHeader)); len(got) != 1 || got[0] != span.SpanContext.SpanID().String() {
		t.Fatalf("response metadata %s = %v, want %s", defaultSpanIDHeader, got, span.SpanContext.SpanID())
	}
}

func TestPanickingCallKeepsTheSpanAndTheWireStatusInAgreement(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	conn := startWireServing(t, &wireImpl{panicOn: "boom"}, i)

	response := new(wrapperspb.StringValue)
	err := conn.Invoke(context.Background(), wireEchoMethod, wrapperspb.String("boom"), response)
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != internalStatusMessage {
		t.Fatalf("wire error = %v, want Internal %q", err, internalStatusMessage)
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if got, ok := spanAttribute(span.Attributes, semconv.RPCResponseStatusCodeKey); !ok || got.AsString() != codes.Internal.String() {
		t.Fatalf("rpc.response.status_code = %v, present=%v", got, ok)
	}
	if span.Status.Code != otelcodes.Error || span.Status.Description != internalStatusMessage || len(span.Events) == 0 {
		t.Fatalf("panic span status/events = %#v/%#v", span.Status, span.Events)
	}
}

func TestStreamingCallSharesOneInterceptedSpan(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	conn := startWireServing(t, &wireImpl{}, i)

	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", incomingTraceparent)
	var header metadata.MD
	stream, err := conn.NewStream(ctx,
		&grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true},
		wireRepeatMethod, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String("hello")); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var received []string
	for {
		message := new(wrapperspb.StringValue)
		if err := stream.RecvMsg(message); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		received = append(received, message.GetValue())
	}
	if len(received) != 3 {
		t.Fatalf("received = %v", received)
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != wireRepeatMethod || span.SpanKind != trace.SpanKindServer {
		t.Fatalf("span = %q, kind %v", span.Name, span.SpanKind)
	}
	if got, ok := spanAttribute(span.Attributes, semconv.RPCMethodKey); !ok || got.AsString() != "Repeat" {
		t.Fatalf("rpc.method = %v, present=%v", got, ok)
	}
	if got := header.Get(strings.ToLower(defaultSpanIDHeader)); len(got) != 1 || got[0] != span.SpanContext.SpanID().String() {
		t.Fatalf("response metadata %s = %v, want %s", defaultSpanIDHeader, got, span.SpanContext.SpanID())
	}
}
