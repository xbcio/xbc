package log

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
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

// RequestID 与 TraceID 必须是同一个值的两种编码，Extract 的反推依赖这条。
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

	//lint:ignore SA1012 显式验证 nil ctx 不 panic
	assert.NotPanics(t, func() { TraceFrom(nil) }) //nolint:staticcheck
	assert.False(t, TraceFrom(nil).Valid())        //nolint:staticcheck
}

func TestNewSpanIDIsUnique(t *testing.T) {
	a, b := newSpanID(), newSpanID()
	assert.True(t, a.IsValid())
	assert.NotEqual(t, a, b)
}
