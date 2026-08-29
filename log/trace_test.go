package log

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
)

func TestNewTraceIsValidAndSampled(t *testing.T) {
	tr := NewTrace("http.request")
	assert.True(t, tr.Valid())
	assert.True(t, tr.TraceID().IsValid())
	assert.True(t, tr.SpanID().IsValid())
	assert.True(t, tr.IsSampled())
	assert.Equal(t, "http.request", tr.SpanName)
	assert.False(t, tr.ParentSpanID.IsValid(), "Root span has no parent")
}

// RequestID and TraceID must be two encodings of the same value; Extract's reverse derivation depends on this.
func TestRequestIDAndTraceIDAreSameValue(t *testing.T) {
	tr := NewTrace("root")

	u, err := ulid.Parse(tr.RequestID)
	require.NoError(t, err, "RequestID must be a valid ULID")
	assert.Equal(t, trace.TraceID(u), tr.TraceID())
	assert.Equal(t, tr.RequestID, ulid.ULID(tr.TraceID()).String(), "Reverse must be lossless")
}

func TestNewTraceIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewTrace("x").TraceID().String()
		_, dup := seen[id]
		require.False(t, dup, "Duplicate trace_id generated on the %d-th attempt", i)
		seen[id] = struct{}{}
	}
}

func TestForkInheritsTraceIDAndChainsParent(t *testing.T) {
	root := NewTrace("http.request")
	child := root.Fork("db.query")

	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id must be inherited")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id must be new")
	assert.Equal(t, root.SpanID(), child.ParentSpanID, "parent must point to the source of the fork")
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID, "request_id spans the entire trace")

	grand := child.Fork("redis.get")
	assert.Equal(t, root.TraceID(), grand.TraceID())
	assert.Equal(t, child.SpanID(), grand.ParentSpanID)
}

func TestForkDoesNotMutateParent(t *testing.T) {
	root := NewTrace("root")
	rootSpan := root.SpanID()
	_ = root.Fork("child")
	assert.Equal(t, rootSpan, root.SpanID(), "Fork is value semantics, cannot be modified by the caller")
}

func TestTraceContextRoundTrip(t *testing.T) {
	want := NewTrace("svc")
	ctx := WithTrace(context.Background(), want)
	got := TraceFrom(ctx)

	assert.Equal(t, want.TraceID(), got.TraceID())
	assert.Equal(t, want.SpanID(), got.SpanID())
	assert.Equal(t, want.RequestID, got.RequestID)
	assert.Equal(t, want.SpanName, got.SpanName)
}

func TestTraceFromMissingReturnsZeroValue(t *testing.T) {
	assert.False(t, TraceFrom(context.Background()).Valid())

	//lint:ignore SA1012 explicitly verifying that a nil ctx does not panic
	assert.NotPanics(t, func() { TraceFrom(nil) }) //nolint:staticcheck
	assert.False(t, TraceFrom(nil).Valid())        //nolint:staticcheck
}

// TraceFrom must interoperate with a ctx that only carries an OTel
// SpanContext -- set purely through the OTel SDK's own API, never touching
// this package's WithTrace.
func TestTraceFromFallsBackToOTelSpanContext(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01},
		SpanID:     trace.SpanID{0x02},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.Equal(t, sc.TraceID(), tr.TraceID())
	assert.Equal(t, tr.TraceID(), trace.TraceID(ulid.MustParse(tr.RequestID)), "request_id is the ULID encoding of trace_id")
}

// The most important regression guard: when ctx carries both an outer OTel
// SpanContext (e.g. from otelhttp instrumentation that was never wired up
// via UseTracer) and an inner local Trace (from this package's own Span),
// TraceFrom must return the local one -- otherwise the inner span would be
// silently swallowed by the outer one, making logging coarser instead of
// finer at exactly the point the caller asked for more detail.
func TestTraceFromPrefersLocalOverOTel(t *testing.T) {
	outer := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa},
		SpanID:     trace.SpanID{0xbb},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), outer)

	inner := NewTrace("db.query")
	ctx = WithTrace(ctx, inner)

	got := TraceFrom(ctx)
	assert.Equal(t, inner.TraceID(), got.TraceID(), "Must be a local trace, not an outer OTel span")
	assert.Equal(t, inner.SpanID(), got.SpanID())
	assert.Equal(t, inner.RequestID, got.RequestID)
	assert.Equal(t, "db.query", got.SpanName)
	assert.NotEqual(t, outer.TraceID(), got.TraceID())
}

func TestNewSpanIDIsUnique(t *testing.T) {
	a, b := newSpanID(), newSpanID()
	assert.True(t, a.IsValid())
	assert.NotEqual(t, a, b)
}

// ── Span tests (Task 8) ─────────────────────────────────────

func TestSpanForksFromParent(t *testing.T) {
	root := NewTrace("http.request")
	ctx := WithTrace(context.Background(), root)

	ctx2, done := Span(ctx, "db.query")
	defer done()

	child := TraceFrom(ctx2)
	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id must be inherited")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id must be new")
	assert.Equal(t, root.SpanID(), child.ParentSpanID)
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID)
}

func TestSpanWithoutParentStartsNewTrace(t *testing.T) {
	ctx, done := Span(context.Background(), "cron.cleanup")
	defer done()

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.False(t, tr.ParentSpanID.IsValid(), "No upstream means root span")
	assert.Equal(t, "cron.cleanup", tr.SpanName)
}

func TestSpanBindsLoggerIntoContext(t *testing.T) {
	logs := installObserver(t)

	ctx, done := Span(context.Background(), "svc.call")
	Ctx(ctx).Info("Inside span")
	done()

	require.GreaterOrEqual(t, len(logs.All()), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, TraceFrom(ctx).TraceID().String(), m["trace_id"])
	assert.Equal(t, "svc.call", m["span_name"])
}

func TestSpanIDOptionOverridesGenerated(t *testing.T) {
	want := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	ctx, done := SpanWith(context.Background(), "custom", SpanID(want))
	defer done()
	assert.Equal(t, want, TraceFrom(ctx).SpanID())
}

func TestSpanIDOptionIgnoresZeroValue(t *testing.T) {
	ctx, done := SpanWith(context.Background(), "x", SpanID(trace.SpanID{}))
	defer done()
	assert.True(t, TraceFrom(ctx).SpanID().IsValid(), "Zero-value span_id should not override the generated one")
}

func TestSpanLogsElapsedOnDone(t *testing.T) {
	logs := installObserver(t)

	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	fake := base
	defer setNow(func() time.Time { return fake })()

	_, done := Span(context.Background(), "slow.op")
	fake = base.Add(1500 * time.Millisecond)
	done()

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, zapcore.DebugLevel, e.Level)
	assert.Equal(t, "span ended", e.Message)
	assert.Equal(t, "slow.op", e.ContextMap()["span_name"])
	assert.InDelta(t, 1500.0, e.ContextMap()["elapsed_ms"], 0.001)
}

func TestSpanNestsThreeLevels(t *testing.T) {
	c1, d1 := Span(context.Background(), "a")
	c2, d2 := Span(c1, "b")
	c3, d3 := Span(c2, "c")
	d3()
	d2()
	d1()

	t1, t2, t3 := TraceFrom(c1), TraceFrom(c2), TraceFrom(c3)
	assert.Equal(t, t1.TraceID(), t3.TraceID(), "Three layers share the same trace_id")
	assert.Equal(t, t1.SpanID(), t2.ParentSpanID)
	assert.Equal(t, t2.SpanID(), t3.ParentSpanID)
}
