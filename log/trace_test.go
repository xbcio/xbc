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
	assert.False(t, tr.ParentSpanID.IsValid(), "根 span 没有 parent")
}

// RequestID and TraceID must be two encodings of the same value; Extract's reverse derivation depends on this.
func TestRequestIDAndTraceIDAreSameValue(t *testing.T) {
	tr := NewTrace("root")

	u, err := ulid.Parse(tr.RequestID)
	require.NoError(t, err, "RequestID 必须是合法 ULID")
	assert.Equal(t, trace.TraceID(u), tr.TraceID())
	assert.Equal(t, tr.RequestID, ulid.ULID(tr.TraceID()).String(), "反推必须无损")
}

func TestNewTraceIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewTrace("x").TraceID().String()
		_, dup := seen[id]
		require.False(t, dup, "第 %d 次生成了重复 trace_id", i)
		seen[id] = struct{}{}
	}
}

func TestForkInheritsTraceIDAndChainsParent(t *testing.T) {
	root := NewTrace("http.request")
	child := root.Fork("db.query")

	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id 必须继承")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id 必须是新的")
	assert.Equal(t, root.SpanID(), child.ParentSpanID, "parent 必须指向 fork 的源")
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID, "request_id 贯穿整条链路")

	grand := child.Fork("redis.get")
	assert.Equal(t, root.TraceID(), grand.TraceID())
	assert.Equal(t, child.SpanID(), grand.ParentSpanID)
}

func TestForkDoesNotMutateParent(t *testing.T) {
	root := NewTrace("root")
	rootSpan := root.SpanID()
	_ = root.Fork("child")
	assert.Equal(t, rootSpan, root.SpanID(), "Fork 是值语义，不能改到调用者")
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
	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id 必须继承")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id 必须是新的")
	assert.Equal(t, root.SpanID(), child.ParentSpanID)
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID)
}

func TestSpanWithoutParentStartsNewTrace(t *testing.T) {
	ctx, done := Span(context.Background(), "cron.cleanup")
	defer done()

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.False(t, tr.ParentSpanID.IsValid(), "没有上游就是根 span")
	assert.Equal(t, "cron.cleanup", tr.SpanName)
}

func TestSpanBindsLoggerIntoContext(t *testing.T) {
	logs := installObserver(t)

	ctx, done := Span(context.Background(), "svc.call")
	Ctx(ctx).Info("span 内部")
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
	assert.True(t, TraceFrom(ctx).SpanID().IsValid(), "零值 span_id 不该覆盖掉生成的那个")
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
	assert.Equal(t, "span 结束", e.Message)
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
	assert.Equal(t, t1.TraceID(), t3.TraceID(), "三层共用一个 trace_id")
	assert.Equal(t, t1.SpanID(), t2.ParentSpanID)
	assert.Equal(t, t2.SpanID(), t3.ParentSpanID)
}
