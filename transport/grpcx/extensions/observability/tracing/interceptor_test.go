package tracing

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	otelcodes "go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
)

const (
	testFullMethod      = "/xbc.test.Tracing/Echo"
	incomingTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpanID      = "00f067aa0ba902b7"
	incomingTraceparent = "00-" + incomingTraceID + "-" + incomingSpanID + "-01"
)

// fakeTransportStream captures what the interceptor writes as a unary
// response header, the way grpc-go's transport does.
type fakeTransportStream struct {
	mu      sync.Mutex
	headers []metadata.MD
}

func (*fakeTransportStream) Method() string { return testFullMethod }

func (f *fakeTransportStream) SetHeader(md metadata.MD) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, md)
	return nil
}

func (f *fakeTransportStream) SendHeader(md metadata.MD) error { return f.SetHeader(md) }

func (*fakeTransportStream) SetTrailer(metadata.MD) error { return nil }

func (f *fakeTransportStream) rawHeaders() []metadata.MD {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]metadata.MD(nil), f.headers...)
}

// fakeServerStream is the stream counterpart: it carries the context the
// stream was accepted with and records header writes.
type fakeServerStream struct {
	ctx context.Context

	mu      sync.Mutex
	headers []metadata.MD
}

func (s *fakeServerStream) SetHeader(md metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = append(s.headers, md)
	return nil
}

func (s *fakeServerStream) SendHeader(md metadata.MD) error { return s.SetHeader(md) }

func (*fakeServerStream) SetTrailer(metadata.MD) {}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

func (*fakeServerStream) SendMsg(any) error { return nil }

func (*fakeServerStream) RecvMsg(any) error { return nil }

func (s *fakeServerStream) rawHeaders() []metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]metadata.MD(nil), s.headers...)
}

// incomingContext carries the W3C trace context the interceptor extracts.
func incomingContext() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", incomingTraceparent))
}

// assertLowercaseKeys pins that every written metadata key is lower case, the
// only spelling gRPC accepts on the wire.
func assertLowercaseKeys(t *testing.T, headers []metadata.MD) {
	t.Helper()
	for _, md := range headers {
		for key := range md {
			if key != strings.ToLower(key) {
				t.Fatalf("written metadata key %q is not lower case", key)
			}
		}
	}
}

func TestUnaryInterceptorOpensAServerSpanAndWritesCorrelationMetadata(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	stream := new(fakeTransportStream)
	ctx := grpc.NewContextWithServerTransportStream(incomingContext(), stream)

	response, err := i.UnaryInterceptor()(ctx, "hello", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(ctx context.Context, req any) (any, error) {
			if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
				t.Error("handler context carries no span")
			}
			return "resp:" + req.(string), nil
		})
	if err != nil || response != "resp:hello" {
		t.Fatalf("response = %v, %v", response, err)
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != testFullMethod {
		t.Fatalf("span name = %q, want the full method", span.Name)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Fatalf("span kind = %v", span.SpanKind)
	}
	wantTraceID, err := trace.TraceIDFromHex(incomingTraceID)
	if err != nil {
		t.Fatal(err)
	}
	wantParentID, err := trace.SpanIDFromHex(incomingSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if span.SpanContext.TraceID() != wantTraceID {
		t.Fatalf("span trace ID = %s, want the extracted %s", span.SpanContext.TraceID(), wantTraceID)
	}
	if span.Parent.SpanID() != wantParentID || !span.Parent.IsRemote() {
		t.Fatalf("span parent = %v, want remote %s", span.Parent, wantParentID)
	}
	if got, ok := spanAttribute(span.Attributes, semconv.RPCSystemNameKey); !ok || got.AsString() != "grpc" {
		t.Fatalf("rpc.system.name = %v, present=%v", got, ok)
	}
	if got, ok := spanAttribute(span.Attributes, semconv.RPCMethodKey); !ok || got.AsString() != "Echo" {
		t.Fatalf("rpc.method = %v, present=%v", got, ok)
	}
	if got, ok := spanAttribute(span.Attributes, semconv.RPCResponseStatusCodeKey); !ok || got.AsString() != codes.OK.String() {
		t.Fatalf("rpc.response.status_code = %v, present=%v", got, ok)
	}

	headers := stream.rawHeaders()
	if len(headers) != 1 {
		t.Fatalf("written header blocks = %d, want 1", len(headers))
	}
	assertLowercaseKeys(t, headers)
	if got := headers[0].Get(strings.ToLower(defaultTraceIDHeader)); len(got) != 1 || got[0] != span.SpanContext.TraceID().String() {
		t.Fatalf("response %s = %v, span = %s", defaultTraceIDHeader, got, span.SpanContext.TraceID())
	}
	if got := headers[0].Get(strings.ToLower(defaultSpanIDHeader)); len(got) != 1 || got[0] != span.SpanContext.SpanID().String() {
		t.Fatalf("response %s = %v, span = %s", defaultSpanIDHeader, got, span.SpanContext.SpanID())
	}
}

func TestUnaryInterceptorRecordsAFailureStatus(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	stream := new(fakeTransportStream)
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), stream)

	wantErr := status.Error(codes.NotFound, "no such echo")
	_, err := i.UnaryInterceptor()(ctx, "hello", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(context.Context, any) (any, error) { return nil, wantErr })
	if err != wantErr {
		t.Fatalf("interceptor returned %v, want the handler error unchanged", err)
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if got, ok := spanAttribute(span.Attributes, semconv.RPCResponseStatusCodeKey); !ok || got.AsString() != codes.NotFound.String() {
		t.Fatalf("rpc.response.status_code = %v, present=%v", got, ok)
	}
	if span.Status.Code != otelcodes.Error || span.Status.Description != "no such echo" || len(span.Events) == 0 {
		t.Fatalf("span status/events = %#v/%#v", span.Status, span.Events)
	}
	// Correlation metadata is written before the handler runs, so a failing
	// call still carries it.
	assertLowercaseKeys(t, stream.rawHeaders())
	if len(stream.rawHeaders()) != 1 {
		t.Fatalf("written header blocks = %d, want 1", len(stream.rawHeaders()))
	}
}

func TestUnaryInterceptorDoesNotWriteMetadataWithoutASpan(t *testing.T) {
	// A provider-less interceptor must neither fail the call nor claim
	// correlation identifiers it does not have.
	i := newInterceptor(nil, tracingcore.ResponseHeaders{Enabled: true, TraceIDHeader: "x-trace-id", SpanIDHeader: "x-span-id"})
	stream := new(fakeTransportStream)
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), stream)

	response, err := i.UnaryInterceptor()(ctx, "hello", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
		func(ctx context.Context, req any) (any, error) {
			if trace.SpanFromContext(ctx).SpanContext().IsValid() {
				t.Error("handler context carries a span without a provider")
			}
			return "resp:" + req.(string), nil
		})
	if err != nil || response != "resp:hello" {
		t.Fatalf("response = %v, %v", response, err)
	}
	if headers := stream.rawHeaders(); len(headers) != 0 {
		t.Fatalf("wrote metadata without a span: %v", headers)
	}
}

func TestUnaryInterceptorRecordsAndRethrowsPanic(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	stream := new(fakeTransportStream)
	ctx := grpc.NewContextWithServerTransportStream(incomingContext(), stream)

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		_, _ = i.UnaryInterceptor()(ctx, "hello", &grpc.UnaryServerInfo{FullMethod: testFullMethod},
			func(context.Context, any) (any, error) { panic("boom") })
	}()

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

func TestStreamInterceptorCarriesTheDerivedContext(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	stream := &fakeServerStream{ctx: incomingContext()}

	var handlerSpan trace.SpanContext
	err := i.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: testFullMethod},
		func(_ any, ss grpc.ServerStream) error {
			if ss.Context() == stream.ctx {
				t.Error("handler saw the accepted context, not the derived one")
			}
			handlerSpan = trace.SpanFromContext(ss.Context()).SpanContext()
			return nil
		})
	if err != nil {
		t.Fatalf("stream interceptor returned %v", err)
	}

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != testFullMethod || span.SpanKind != trace.SpanKindServer {
		t.Fatalf("span = %q, kind %v", span.Name, span.SpanKind)
	}
	if !handlerSpan.IsValid() || handlerSpan.SpanID() != span.SpanContext.SpanID() {
		t.Fatalf("handler span = %v, exported = %v", handlerSpan, span.SpanContext)
	}
	wantParentID, err := trace.SpanIDFromHex(incomingSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if span.Parent.SpanID() != wantParentID || !span.Parent.IsRemote() {
		t.Fatalf("span parent = %v, want remote %s", span.Parent, wantParentID)
	}
	headers := stream.rawHeaders()
	if len(headers) != 1 {
		t.Fatalf("written header blocks = %d, want 1", len(headers))
	}
	assertLowercaseKeys(t, headers)
	if got := headers[0].Get(strings.ToLower(defaultSpanIDHeader)); len(got) != 1 || got[0] != span.SpanContext.SpanID().String() {
		t.Fatalf("response %s = %v, span = %s", defaultSpanIDHeader, got, span.SpanContext.SpanID())
	}
}

func TestStreamInterceptorRecordsAndRethrowsPanic(t *testing.T) {
	i, handle, exporter := newTestInterceptor(t)
	stream := &fakeServerStream{ctx: incomingContext()}

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		_ = i.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: testFullMethod},
			func(any, grpc.ServerStream) error { panic("boom") })
	}()

	spans := flush(t, handle, exporter)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if got, ok := spanAttribute(span.Attributes, semconv.RPCResponseStatusCodeKey); !ok || got.AsString() != codes.Internal.String() {
		t.Fatalf("rpc.response.status_code = %v, present=%v", got, ok)
	}
	if span.Status.Code != otelcodes.Error || len(span.Events) == 0 {
		t.Fatalf("panic span status/events = %#v/%#v", span.Status, span.Events)
	}
}
