package tracing

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// internalStatusMessage is what the Server's panic boundary answers a
// re-panicked call with. The span records the same fact so a trace reader sees
// the failure the caller will.
const internalStatusMessage = "internal error"

// Order implements grpctransport.UnaryInterceptor and
// grpctransport.StreamInterceptor. Both chains run observation first, ahead of
// metrics, so the sibling interceptors observe a call that already carries an
// active span.
func (*Interceptor) Order() grpctransport.Order {
	return grpctransport.Order{
		Phase:  grpctransport.PhaseObserve,
		Before: []grpctransport.OrderRef{grpctransport.Prefer(metricsKey)},
	}
}

// UnaryInterceptor implements grpctransport.UnaryInterceptor.
func (i *Interceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		ctx, span := i.start(ctx, info.FullMethod)
		if headers := i.responseHeaderMetadata(span); headers != nil {
			_ = grpc.SetHeader(ctx, headers)
		}
		defer func() {
			panicValue := recover()
			i.finish(span, err, panicValue)
			if panicValue != nil {
				// Recorded, not swallowed: the Server's panic boundary is the
				// stage that turns a panic into a status, and it is assembled
				// outside every contributed interceptor.
				panic(panicValue)
			}
		}()
		return handler(ctx, req)
	}
}

// StreamInterceptor implements grpctransport.StreamInterceptor. A stream's
// messages travel under the span the stream opened with; the handler sees the
// derived context through ServerStream.Context().
func (i *Interceptor) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx, span := i.start(ss.Context(), info.FullMethod)
		if headers := i.responseHeaderMetadata(span); headers != nil {
			_ = ss.SetHeader(headers)
		}
		wrapped := &contextStream{ServerStream: ss, ctx: ctx}
		defer func() {
			panicValue := recover()
			i.finish(span, err, panicValue)
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		return handler(srv, wrapped)
	}
}

// start extracts the incoming W3C context and opens one server span named
// after the full method. A zero handle or a provider that yields no tracer
// degrades to the context's own span, which is a no-op: an interceptor must
// never make a call fail because tracing is absent.
func (i *Interceptor) start(ctx context.Context, fullMethod string) (context.Context, trace.Span) {
	if i == nil || i.handle == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	tracer := i.handle.Tracer(instrumentationName)
	if tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	md, _ := metadata.FromIncomingContext(ctx)
	parent := i.handle.Extract(ctx, metadataCarrier{md: md})
	ctx, span := tracer.Start(parent, fullMethod,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.RPCSystemNameGRPC,
			// This semconv version has no rpc.service key; the service name
			// is what the span name carries.
			semconv.RPCMethod(shortMethod(fullMethod)),
		),
	)
	return ctx, span
}

// finish labels the span with the final status and ends it. A panic is
// recorded as codes.Internal with the panic boundary's message, because
// re-panicking means the boundary, not this interceptor, decides the wire
// status.
func (i *Interceptor) finish(span trace.Span, err error, panicValue any) {
	if span == nil {
		return
	}
	code := status.Code(err)
	if panicValue != nil {
		code = codes.Internal
	}
	span.SetAttributes(semconv.RPCResponseStatusCode(code.String()))
	switch {
	case panicValue != nil:
		span.RecordError(panicAsError(panicValue), trace.WithStackTrace(true))
		span.SetStatus(otelcodes.Error, internalStatusMessage)
	case err != nil:
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, status.Convert(err).Message())
	}
	span.End()
}

// responseHeaderMetadata is the trace correlation metadata written before the
// handler runs, the gRPC counterpart of the Web interceptor's response
// headers. A nil result writes nothing. The SetHeader error is deliberately
// ignored: it can only fire when the RPC has no transport stream, which a real
// call always has.
func (i *Interceptor) responseHeaderMetadata(span trace.Span) metadata.MD {
	if i == nil || !i.responseHeaders.Enabled || span == nil {
		return nil
	}
	spanContext := span.SpanContext()
	if !spanContext.IsValid() {
		return nil
	}
	return metadata.Pairs(
		i.responseHeaders.TraceIDHeader, spanContext.TraceID().String(),
		i.responseHeaders.SpanIDHeader, spanContext.SpanID().String(),
	)
}

// shortMethod is the method name without its package-qualified service, which
// is what the rpc.method attribute carries. The full method is what the span
// name carries instead of an rpc.service attribute this semconv version does
// not define.
func shortMethod(fullMethod string) string {
	if index := strings.LastIndex(fullMethod, "/"); index >= 0 {
		return fullMethod[index+1:]
	}
	return fullMethod
}

func panicAsError(value any) error {
	if err, ok := value.(error); ok {
		return err
	}
	return fmt.Errorf("panic: %v", value)
}

// metadataCarrier adapts incoming gRPC metadata to the propagation
// TextMapCarrier contract. Only reads are meaningful: this interceptor
// extracts a parent context and writes its own correlation metadata directly,
// so Set is never reached.
type metadataCarrier struct {
	md metadata.MD
}

var _ propagation.TextMapCarrier = metadataCarrier{}

func (c metadataCarrier) Get(key string) string {
	values := c.md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (metadataCarrier) Set(string, string) {}

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c.md))
	for key := range c.md {
		keys = append(keys, key)
	}
	return keys
}

// contextStream carries the context the interceptor derived, so stream
// handlers observe the started span and the extracted parent through
// ServerStream.Context().
type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextStream) Context() context.Context { return s.ctx }
