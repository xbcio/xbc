package log

import (
	"context"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel/trace"
)

// Trace is the tracing context of a single call.
//
// It embeds OTel's trace.SpanContext, so it can be fed directly into the OTel
// SDK and can also be populated from values parsed out of a W3C traceparent
// header -- no conversion layer required.
//
// TraceID and RequestID are two encodings of the same 128-bit value:
// the former is the 32-hex-digit form required by W3C, the latter is the
// 26-character Crockford Base32 form used by ULID.
// The first 6 bytes of a ULID are a millisecond timestamp, so request_id is
// naturally time-ordered and can be compared by eye.
type Trace struct {
	trace.SpanContext

	// ParentSpanID is the upstream span. A root span has the zero value.
	ParentSpanID trace.SpanID

	// SpanName is the name of the current span, e.g. "GET /orders/:id", "db.query".
	SpanName string

	// RequestID is the ULID encoding of TraceID, unchanged throughout the whole chain.
	RequestID string
}

type traceKey struct{}

// NewTrace starts a new trace.
func NewTrace(name string) Trace {
	id := ulid.Make()
	return Trace{
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID(id),
			SpanID:     newSpanID(),
			TraceFlags: trace.FlagsSampled,
		}),
		SpanName:  name,
		RequestID: id.String(),
	}
}

// newSpanID generates an 8-byte span_id.
// Takes ULID's [8:16] -- a ULID's layout is 6 bytes of timestamp + 10 bytes
// of random entropy, and this slice falls entirely within the random region,
// which is enough for our purposes and avoids depending directly on crypto/rand.
func newSpanID() trace.SpanID {
	u := ulid.Make()
	return trace.SpanID(u[8:])
}

// Valid reports whether this trace is valid. The zero-value Trace returns false.
func (t Trace) Valid() bool { return t.TraceID().IsValid() }

// Fork derives a child span: trace_id and request_id stay the same, span_id
// is regenerated, and parent points at the current span. Value semantics -- the caller is not mutated.
func (t Trace) Fork(name string) Trace {
	child := t
	child.ParentSpanID = t.SpanID()
	child.SpanContext = t.SpanContext.WithSpanID(newSpanID())
	child.SpanName = name
	return child
}

// WithTrace stores the trace into ctx.
func WithTrace(ctx context.Context, t Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom retrieves the trace from ctx. Returns the zero-value Trace (Valid() == false) if not found, without panicking.
func TraceFrom(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	t, _ := ctx.Value(traceKey{}).(Trace)
	return t
}
