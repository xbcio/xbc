package log

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// resetTraceConfig restores the pre-Init layout for tests that publish a
// custom one.
func resetTraceConfig() { traceConfigState.Store(nil) }

// TestTraceFieldNamesFollowConfig pins the runtime half of Config.Trace: an
// attached sink receives the trace context under the configured names, not the
// built-in ones.
func TestTraceFieldNamesFollowConfig(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)

	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	cfg.Trace = TraceConfig{
		TraceID:   "traceID",
		SpanID:    "spanID",
		SpanName:  "operation",
		RequestID: "requestID",
	}
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
		resetTraceConfig()
	})

	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()

	tr := NewTrace("orders.create")
	TInfo(WithTrace(context.Background(), tr), "custom layout")

	entries := rec.recorded()
	require.Len(t, entries, 1)
	assert.Equal(t, tr.TraceID().String(), entries[0]["traceID"])
	assert.Equal(t, tr.SpanID().String(), entries[0]["spanID"])
	assert.Equal(t, "orders.create", entries[0]["operation"])
	assert.Equal(t, tr.RequestID, entries[0]["requestID"])
	for _, builtin := range []string{"trace_id", "span_id", "span_name", "request_id"} {
		assert.NotContains(t, entries[0], builtin, "the built-in name must not be emitted alongside the configured one")
	}
}

// TestConsoleEncoderFollowsConfiguredTraceField pins that the console encoder
// tracks a renamed trace ID in its fixed column and keeps the configured ID
// fields out of the KV section, exactly as it does for the built-in names.
func TestConsoleEncoderFollowsConfiguredTraceField(t *testing.T) {
	names := TraceConfig{
		TraceID:   "traceID",
		SpanID:    "spanID",
		SpanName:  "operation",
		RequestID: "requestID",
	}
	const traceID = "01926f7e1a2b3c4d5e6f708192a3b4c5"

	enc := newConsoleEncoderWithTrace(false, names)
	ent := zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Date(2026, 8, 24, 10, 23, 45, 123_000_000, time.Local),
		Message: "renamed",
	}
	buf, err := enc.EncodeEntry(ent, []zapcore.Field{
		zap.String(names.TraceID, traceID),
		zap.String(names.SpanID, "01926f7e1a2b3c4d"),
		zap.String(names.RequestID, "01J0R6RF8V0YQ6P4W3N2K1M0Z9"),
		zap.String(names.SpanName, "orders.create"),
		zap.Int("order_id", 1001),
	})
	require.NoError(t, err)
	out := buf.String()

	assert.Contains(t, out, "01926f7e", "the fixed trace column must read the configured name")
	assert.NotContains(t, out, traceID, "the trace ID stays a short column, not a raw KV value")
	assert.NotContains(t, out, names.TraceID+"=")
	assert.NotContains(t, out, names.SpanID+"=")
	assert.NotContains(t, out, names.RequestID+"=")
	assert.Contains(t, out, names.SpanName+"=orders.create", "only the ID fields are hidden from the KV section")
	assert.Contains(t, out, "order_id=1001")
}
