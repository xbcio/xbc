package log

import (
	"context"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is this framework's extension header for the request ID.
// W3C only specifies traceparent; request_id is the human-friendly half.
const RequestIDHeader = "X-Request-Id"

// Only W3C TraceContext is used. Formats like B3 or Jaeger can be wired up
// by the caller if needed.
var propagator = propagation.TraceContext{}

// Extract decodes the upstream trace from the inbound carrier, opens a new
// span for this process, and binds a Logger carrying the trace fields into
// ctx.
//
// If upstream doesn't provide a valid traceparent, a new trace is started --
// a malformed header shouldn't fail the request.
//
// carrier uses propagation.TextMapCarrier instead of http.Header: the log
// package shouldn't know about HTTP. The HTTP layer wraps it with
// propagation.HeaderCarrier, and gRPC uses a metadata adapter -- both go
// through the same interface.
func Extract(ctx context.Context, carrier propagation.TextMapCarrier, spanName string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	var t Trace
	if sc := trace.SpanContextFromContext(propagator.Extract(ctx, carrier)); sc.IsValid() {
		t = Trace{
			SpanContext:  sc.WithSpanID(newSpanID()),
			ParentSpanID: sc.SpanID(),
			SpanName:     spanName,
			RequestID:    requestIDFrom(carrier, sc.TraceID()),
		}
	} else {
		t = NewTrace(spanName)
	}

	return NewContext(WithTrace(ctx, t), L().With(traceKV(t)...))
}

// requestIDFrom prefers the upstream-supplied X-Request-Id; falling back to
// deriving it from trace_id when absent.
// trace_id and ULID are both 128 bits, two encodings of the same value, so
// the derivation is lossless.
func requestIDFrom(carrier propagation.TextMapCarrier, tid trace.TraceID) string {
	if v := carrier.Get(RequestIDHeader); v != "" {
		return v
	}
	return ulid.ULID(tid).String()
}

// Inject writes the current trace into the outbound carrier: W3C traceparent
// plus X-Request-Id.
// Does nothing when ctx has no trace -- don't manufacture an invalid
// traceparent.
func Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	t := TraceFrom(ctx)
	if !t.Valid() {
		return
	}
	propagator.Inject(trace.ContextWithSpanContext(ctx, t.SpanContext), carrier)
	if t.RequestID != "" {
		carrier.Set(RequestIDHeader, t.RequestID)
	}
}
