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

// ── Span ─────────────────────────────────────────────────

type spanOptions struct {
	spanID trace.SpanID
}

// SpanOption adjusts how a span is created.
type SpanOption func(*spanOptions)

// SpanID sets the span_id, used to align with an external system. A zero
// value is ignored.
func SpanID(id trace.SpanID) SpanOption {
	return func(o *spanOptions) { o.spanID = id }
}

// Span opens a child span, returning a ctx carrying the trace and a done
// callback.
//
//	ctx, done := log.Span(ctx, "db.query")
//	defer done()
//
// The ctx already has a Logger bound to it with trace fields attached, so
// subsequent log.TInfo(ctx, ...) calls retrieve it with zero allocations.
//
// Pre-Init behavior: Span uses L() to obtain the logger. Before Init or
// SetLogger has been called, L() returns Nop(), so the "span 结束" log
// emitted by done() is silently discarded. This is by design — L()'s
// Nop-before-init contract (Task 7) makes all logging a no-op until the
// application explicitly initializes the backend.
//
// done() should be called exactly once (via defer). Calling it more than
// once does not panic or corrupt state, but emits a duplicate "span 结束"
// log line with a different elapsed_ms — observable noise, not a
// correctness issue. Adding sync.Once to prevent this is deliberately
// avoided: Span is a hot path, and allocating a sync.Once per span for a
// misuse guard that only produces one extra log line is not worth the cost.
// The idiomatic usage "defer done()" inherently calls it exactly once.
func Span(ctx context.Context, name string) (context.Context, func()) {
	return SpanWith(ctx, name)
}

// SpanWith is the option-taking version of Span.
func SpanWith(ctx context.Context, name string, opts ...SpanOption) (context.Context, func()) {
	var o spanOptions
	for _, fn := range opts {
		fn(&o)
	}

	var child Trace
	if parent := TraceFrom(ctx); parent.Valid() {
		child = parent.Fork(name)
	} else {
		child = NewTrace(name)
	}
	if o.spanID.IsValid() {
		child.SpanContext = child.SpanContext.WithSpanID(o.spanID)
	}

	l := L().With(traceKV(child)...)
	ctx = NewContext(WithTrace(ctx, child), l)

	start := nowFunc()
	return ctx, func() {
		done := l
		// The callback adds one extra closure frame vs. a normal call; skip one
		// more so the caller points to the function containing defer done()
		if cs, ok := done.(CallerSkipper); ok {
			done = cs.WithCallerSkip(1)
		}
		done.Debug("span 结束", "elapsed_ms",
			float64(nowFunc().Sub(start).Microseconds())/1000)
	}
}
