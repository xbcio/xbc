package log

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"
)

// fieldValues returns every value recorded under key, preserving duplicates so
// a test can prove a field was not stacked.
func fieldValues(entry observer.LoggedEntry, key string) []string {
	var values []string
	for _, field := range entry.Context {
		if field.Key == key {
			values = append(values, field.String)
		}
	}
	return values
}

func TestWithRequestIDSupersedesTraceRequestID(t *testing.T) {
	logs := installObserver(t)
	resetTraceConfig()
	t.Cleanup(resetTraceConfig)

	trace := NewTrace("orders.create")
	ctx := WithTrace(context.Background(), trace)
	ctx = WithRequestID(ctx, "client-123")
	Ctx(ctx).Info("probe")

	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Equal(t, trace.TraceID().String(), entries[0].ContextMap()["trace_id"])
	assert.Equal(t, trace.SpanID().String(), entries[0].ContextMap()["span_id"])
	assert.Equal(t, "client-123", entries[0].ContextMap()["request_id"])
	assert.Equal(t, "client-123", TraceFrom(ctx).RequestID,
		"the trace must adopt the request ID so Span and Inject derive the same value")
	assert.Len(t, fieldValues(entries[0], RequestIDField), 1,
		"the trace-derived request_id must be replaced, not emitted beside the bound one")
}

func TestWithRequestIDKeepsExactlyOneValueAcrossRepeatedCalls(t *testing.T) {
	logs := installObserver(t)

	ctx := WithRequestID(context.Background(), "first")
	ctx = WithRequestID(ctx, "first")
	ctx = WithRequestID(ctx, "second")
	Ctx(ctx).Info("probe")

	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Equal(t, "second", entries[0].ContextMap()["request_id"])
	assert.Len(t, fieldValues(entries[0], RequestIDField), 1,
		"repeated calls must not stack request_id fields")
}

func TestWithRequestIDPreservesABoundLoggersFields(t *testing.T) {
	logs := installObserver(t)

	ctx := NewContext(context.Background(), L().With("tenant", "acme"))
	ctx = WithRequestID(ctx, "client-456")
	Ctx(ctx).Info("probe")

	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Equal(t, "acme", entries[0].ContextMap()["tenant"])
	assert.Equal(t, "client-456", entries[0].ContextMap()["request_id"])
}

func TestWithRequestIDFollowsAConfiguredTraceRequestIDName(t *testing.T) {
	logs := installObserver(t)
	traceConfigState.Store(&TraceConfig{
		TraceID:   "traceID",
		SpanID:    "spanID",
		SpanName:  "operation",
		RequestID: "requestID",
	})
	t.Cleanup(resetTraceConfig)

	ctx := WithTrace(context.Background(), NewTrace("orders.create"))
	ctx = WithRequestID(ctx, "client-789")
	Ctx(ctx).Info("probe")

	entries := logs.All()
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0].ContextMap(), "requestID", "the trace-derived name must be superseded")
	assert.Equal(t, "client-789", entries[0].ContextMap()[RequestIDField])
	assert.Len(t, fieldValues(entries[0], RequestIDField), 1)
}

func TestWithRequestIDEmptyOrNilInput(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, ctx, WithRequestID(ctx, ""), "an empty ID must leave the context unchanged")
	assert.NotNil(t, WithRequestID(nil, "id"), "a nil context must be treated as Background")
}
