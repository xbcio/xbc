package log

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// memorySink is the io.Writer an application-owned JSON core writes into. A
// mutex is not required by Attach's synchronous delivery, but keeps the helper
// usable from a concurrent test without reasoning about it.
type memorySink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *memorySink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *memorySink) Sync() error { return nil }

func (w *memorySink) entries(t *testing.T) []map[string]any {
	t.Helper()
	w.mu.Lock()
	raw := w.buf.String()
	w.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "sink line: %s", line)
		out = append(out, m)
	}
	return out
}

// TestJSONCoreAttachedFromApplicationCode demonstrates the shape an
// application uses to ship every record to an external collector: build a JSON
// core over an io.Writer, fix the encoder's key layout, add static fields with
// With, and Attach it to the running backend.
//
// It also pins the seams an attached sink depends on: trace fields reach the
// core, caller is encoded when the backend collects it, static With fields are
// present, and a core whose enabler admits only Error never receives an Info
// record.
func TestJSONCoreAttachedFromApplicationCode(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)

	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
	})

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:       "timestamp",
		EncodeTime:    zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000Z0700"),
		LevelKey:      "level",
		EncodeLevel:   zapcore.CapitalLevelEncoder,
		MessageKey:    "message",
		CallerKey:     "logger",
		EncodeCaller:  zapcore.ShortCallerEncoder,
		StacktraceKey: "stack",
	}
	staticFields := []zapcore.Field{
		zap.String("appName", "orders"),
		zap.String("host", "host-1"),
	}

	infoSink := &memorySink{}
	errorSink := &memorySink{}
	infoCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(infoSink),
		zapcore.InfoLevel,
	).With(staticFields)
	errorCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(errorSink),
		zapcore.ErrorLevel,
	).With(staticFields)

	detachInfo := Attach(infoCore)
	detachError := Attach(errorCore)
	defer detachInfo()
	defer detachError()

	tr := NewTrace("orders.create")
	ctx := WithTrace(context.Background(), tr)
	TInfo(ctx, "order accepted", "order_id", 1001)
	L().Error("order rejected", "err", "storage full")

	infoEntries := infoSink.entries(t)
	require.Len(t, infoEntries, 2, "an info-level core admits both records")
	info := infoEntries[0]
	assert.NotEmpty(t, info["timestamp"], "custom time key")
	assert.Equal(t, "order accepted", info["message"], "custom message key")
	assert.Equal(t, "INFO", info["level"], "capital level encoder")
	logger, _ := info["logger"].(string)
	assert.Contains(t, logger, "json_sink_test.go", "caller reaches the attached core under its custom key")
	assert.Equal(t, "orders", info["appName"], "a static field fixed with With is present")
	assert.Equal(t, "host-1", info["host"])
	assert.Equal(t, tr.TraceID().String(), info["trace_id"], "trace context reaches the attached core under the configured name")
	assert.EqualValues(t, 1001, info["order_id"])

	errorEntries := errorSink.entries(t)
	require.Len(t, errorEntries, 1, "the error-only core must not receive the info record")
	assert.Equal(t, "order rejected", errorEntries[0]["message"])
	assert.Equal(t, "ERROR", errorEntries[0]["level"])
	assert.Equal(t, "orders", errorEntries[0]["appName"])
}
