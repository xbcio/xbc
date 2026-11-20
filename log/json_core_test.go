package log

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// syncCountingWriter lets a test pin both what the core wrote and whether Sync
// reached the underlying writer.
type syncCountingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	syncs int
}

func (w *syncCountingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncCountingWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncs++
	return nil
}

func (w *syncCountingWriter) syncCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncs
}

func (w *syncCountingWriter) lines(t *testing.T) []map[string]any {
	t.Helper()
	w.mu.Lock()
	raw := w.buf.String()
	w.mu.Unlock()
	return decodeJSONLines(t, raw)
}

func decodeJSONLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "record line: %s", line)
		out = append(out, m)
	}
	return out
}

// TestNewJSONCoreDefaultsMatchJSONEncoderConfig pins the zero value to the
// package's JSON baseline: one shared encoderConfig is the only place the
// defaults live, so a custom core cannot drift from the built-in file sink.
func TestNewJSONCoreDefaultsMatchJSONEncoderConfig(t *testing.T) {
	var got, want bytes.Buffer
	core, err := NewJSONCore(JSONCoreOptions{Writer: &got, Enabler: zapcore.DebugLevel})
	require.NoError(t, err)
	baseline := zapcore.NewCore(
		zapcore.NewJSONEncoder(jsonEncoderConfig()),
		zapcore.AddSync(&want),
		zapcore.DebugLevel,
	)

	entry := zapcore.Entry{
		Level:   zapcore.WarnLevel,
		Time:    time.Date(2024, 1, 2, 3, 4, 5, 678000000, time.UTC),
		Message: "Place an order",
		Caller:  zapcore.EntryCaller{Defined: true, File: "/src/orders/handler.go", Line: 42},
		Stack:   "stack line",
	}
	fields := []zapcore.Field{zap.String("order_id", "A-1"), zap.Int("attempt", 2)}
	require.NoError(t, core.Write(entry, fields))
	require.NoError(t, baseline.Write(entry, fields))

	assert.JSONEq(t, want.String(), got.String(), "the default core must encode exactly like jsonEncoderConfig")

	records := decodeJSONLines(t, got.String())
	require.Len(t, records, 1)
	assert.Equal(t, "2024-01-02T03:04:05.678Z", records[0]["ts"], "RFC3339Nano under the default key")
	assert.Equal(t, "warn", records[0]["level"], "lowercase level")
	assert.Equal(t, "Place an order", records[0]["msg"])
	assert.Equal(t, "orders/handler.go:42", records[0]["caller"], "short caller")
	assert.Equal(t, "stack line", records[0]["stack"])
	assert.EqualValues(t, 2, records[0]["attempt"])
}

// TestNewJSONCoreCustomLayout pins the destination-facing overrides: renamed
// keys, a millisecond time layout, an upper-case level, and the full caller
// path.
func TestNewJSONCoreCustomLayout(t *testing.T) {
	var out bytes.Buffer
	core, err := NewJSONCore(JSONCoreOptions{
		Writer: &out,
		Level:  zapcore.InfoLevel,
		Exact:  true,
		Keys: JSONKeys{
			Time:    "timestamp",
			Level:   "severity",
			Message: "message",
			Caller:  "logger",
			Stack:   "traceback",
		},
		TimeLayout: "2006-01-02T15:04:05.000Z0700",
		UpperLevel: true,
		FullCaller: true,
	})
	require.NoError(t, err)

	entry := zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Date(2024, 1, 2, 3, 4, 5, 678000000, time.UTC),
		Message: "accepted",
		Caller:  zapcore.EntryCaller{Defined: true, File: "/very/long/path/to/pkg/file.go", Line: 7},
		Stack:   "boom",
	}
	require.NoError(t, core.Write(entry, []zapcore.Field{zap.String("order_id", "A-1")}))

	records := decodeJSONLines(t, out.String())
	require.Len(t, records, 1)
	rec := records[0]
	assert.Equal(t, "2024-01-02T03:04:05.678Z", rec["timestamp"])
	assert.Equal(t, "INFO", rec["severity"])
	assert.Equal(t, "accepted", rec["message"])
	assert.Equal(t, "/very/long/path/to/pkg/file.go:7", rec["logger"], "full caller path")
	assert.Equal(t, "boom", rec["traceback"])
	for _, replaced := range []string{"ts", "level", "msg", "caller", "stack"} {
		assert.NotContains(t, rec, replaced, "a replaced default key must not be emitted as well")
	}
}

// TestNewJSONCoreExactRoutesByLevel pins the split an exact core exists for:
// two cores, each on its own topic, must not duplicate records.
func TestNewJSONCoreExactRoutesByLevel(t *testing.T) {
	t.Run("info core refuses error", func(t *testing.T) {
		var out bytes.Buffer
		core, err := NewJSONCore(JSONCoreOptions{Writer: &out, Level: zapcore.InfoLevel, Exact: true})
		require.NoError(t, err)
		logger := zap.New(core, zap.AddCaller())

		logger.Info("info record")
		logger.Error("error record")

		assert.True(t, core.Enabled(zapcore.InfoLevel))
		assert.False(t, core.Enabled(zapcore.ErrorLevel), "an exact info core must not admit error")
		assert.Equal(t, []string{"info record"}, messagesOf(decodeJSONLines(t, out.String())))
	})

	t.Run("error core refuses info", func(t *testing.T) {
		var out bytes.Buffer
		core, err := NewJSONCore(JSONCoreOptions{Writer: &out, Level: zapcore.ErrorLevel, Exact: true})
		require.NoError(t, err)
		logger := zap.New(core, zap.AddCaller())

		logger.Info("info record")
		logger.Error("error record")

		assert.False(t, core.Enabled(zapcore.InfoLevel), "an exact error core must not admit info")
		assert.True(t, core.Enabled(zapcore.ErrorLevel))
		assert.Equal(t, []string{"error record"}, messagesOf(decodeJSONLines(t, out.String())))
	})

	t.Run("range core admits the level and above", func(t *testing.T) {
		core, err := NewJSONCore(JSONCoreOptions{Writer: io.Discard, Level: zapcore.WarnLevel})
		require.NoError(t, err)
		assert.False(t, core.Enabled(zapcore.InfoLevel))
		assert.True(t, core.Enabled(zapcore.WarnLevel))
		assert.True(t, core.Enabled(zapcore.ErrorLevel))
	})

	t.Run("enabler replaces level and exact", func(t *testing.T) {
		core, err := NewJSONCore(JSONCoreOptions{
			Writer:  io.Discard,
			Level:   zapcore.InfoLevel,
			Exact:   true,
			Enabler: zapcore.WarnLevel,
		})
		require.NoError(t, err)
		assert.False(t, core.Enabled(zapcore.InfoLevel))
		assert.True(t, core.Enabled(zapcore.WarnLevel))
		assert.True(t, core.Enabled(zapcore.ErrorLevel), "the enabler's own range applies, not the exact level")
	})
}

// TestNewJSONCoreFields pins that the static fields are evaluated once and
// present on every record, and that they are masked like any other field.
func TestNewJSONCoreFields(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	core, err := NewJSONCore(JSONCoreOptions{
		Writer: &out,
		Fields: func() []zap.Field {
			calls++
			return []zap.Field{
				zap.String("app", "orders"),
				zap.String("host", "host-1"),
				zap.String("password", "hunter2"),
			}
		},
	})
	require.NoError(t, err)
	logger := zap.New(core)

	logger.Info("first")
	logger.Warn("second")

	assert.Equal(t, 1, calls, "Fields must run once at construction, not per record")
	records := decodeJSONLines(t, out.String())
	require.Len(t, records, 2)
	for _, rec := range records {
		assert.Equal(t, "orders", rec["app"])
		assert.Equal(t, "host-1", rec["host"])
		assert.Equal(t, maskPlaceholder, rec["password"], "static fields go through the built-in blacklist")
	}
}

// TestNewJSONCoreValidation covers the configuration errors a caller can make:
// a missing writer, blank and duplicate keys, and keys that collided with the
// trace layout active when the core is built.
func TestNewJSONCoreValidation(t *testing.T) {
	resetTraceConfig()
	t.Cleanup(resetTraceConfig)

	_, err := NewJSONCore(JSONCoreOptions{Writer: io.Discard})
	require.NoError(t, err, "the zero value beyond Writer must be valid")

	cases := []struct {
		name string
		opts JSONCoreOptions
	}{
		{"nil writer", JSONCoreOptions{}},
		{"blank time key", JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Time: "  "}}},
		{"blank message key", JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Message: "\t"}}},
		{"duplicate custom keys", JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Message: "event", Caller: "event"}}},
		{"duplicate with default", JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Level: "ts"}}},
		{"trace conflict", JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Message: "trace_id"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewJSONCore(tc.opts)
			assert.Error(t, err)
			assert.NotContains(t, err.Error(), "hunter2", "errors carry no record data")
		})
	}

	applyTraceConfig(TraceConfig{
		TraceID:   "trace",
		SpanID:    "span",
		SpanName:  "operation",
		RequestID: "request",
	})
	_, err = NewJSONCore(JSONCoreOptions{Writer: io.Discard, Keys: JSONKeys{Level: "operation"}})
	assert.Error(t, err, "a renamed trace field must be caught against the layout active at construction")
}

// TestNewJSONCoreSync pins that a Sync-capable writer is invoked and that a
// plain io.Writer is accepted with a no-op Sync, matching zapcore.AddSync.
func TestNewJSONCoreSync(t *testing.T) {
	w := &syncCountingWriter{}
	core, err := NewJSONCore(JSONCoreOptions{Writer: w})
	require.NoError(t, err)
	zap.New(core).Info("one")

	require.NoError(t, core.Sync())
	assert.Equal(t, 1, w.syncCount(), "the writer's Sync must be called by the core's Sync")

	plain, err := NewJSONCore(JSONCoreOptions{Writer: io.Discard})
	require.NoError(t, err)
	assert.NoError(t, plain.Sync(), "a writer without Sync is accepted and Sync is a no-op")
}

// TestNewJSONCoreThroughAttach pins the seam contract: wiring the core through
// Attach brings this package's masking and the global level gate with it.
func TestNewJSONCoreThroughAttach(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)

	cfg := DefaultConfig()
	cfg.Level = "info"
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
		if _, err := SetLevel("info"); err != nil {
			t.Errorf("restore level: %v", err)
		}
	})

	var out bytes.Buffer
	core, err := NewJSONCore(JSONCoreOptions{Writer: &out})
	require.NoError(t, err)
	detach := Attach(core)
	defer detach()

	L().Info("login", "password", "hunter2", "user", "alice")
	records := decodeJSONLines(t, out.String())
	require.Len(t, records, 1)
	assert.Equal(t, maskPlaceholder, records[0]["password"], "the attached core must see masked values")
	assert.Equal(t, "alice", records[0]["user"])

	_, err = SetLevel("error")
	require.NoError(t, err)
	L().Info("suppressed")
	L().Error("boom")

	records = decodeJSONLines(t, out.String())
	require.Len(t, records, 2, "records below the raised global level must not reach the core")
	assert.Equal(t, []string{"login", "boom"}, messagesOf(records))
}

// TestNewJSONCoreConcurrentWrites exercises the core and the writer from many
// goroutines; atomicity of each JSON line is what the race detector checks.
func TestNewJSONCoreConcurrentWrites(t *testing.T) {
	w := &syncCountingWriter{}
	core, err := NewJSONCore(JSONCoreOptions{Writer: w, Level: zapcore.DebugLevel})
	require.NoError(t, err)
	logger := zap.New(core)

	const goroutines, perGoroutine = 8, 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(worker int) {
			defer wg.Done()
			for seq := 0; seq < perGoroutine; seq++ {
				logger.Info("concurrent", zap.Int("worker", worker), zap.Int("seq", seq))
			}
		}(i)
	}
	wg.Wait()

	records := w.lines(t)
	require.Len(t, records, goroutines*perGoroutine)
	for _, rec := range records {
		assert.Equal(t, "concurrent", rec["msg"])
	}
}
