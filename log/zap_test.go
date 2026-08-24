package log

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// installObserver swaps the global backend for an in-memory observer and
// restores it automatically when the test ends.
func installObserver(t *testing.T, opts ...zap.Option) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	SetLogger(newZapLogger(zap.New(core, opts...)))
	t.Cleanup(func() { SetLogger(Nop()) })
	return logs
}

func TestToFields(t *testing.T) {
	fs := toFields([]any{"a", 1, "b", "x"})
	require.Len(t, fs, 2)
	assert.Equal(t, "a", fs[0].Key)
	assert.Equal(t, "b", fs[1].Key)

	assert.Nil(t, toFields(nil))
	assert.Nil(t, toFields([]any{}))
}

func TestToFieldsOddCountProducesBadKey(t *testing.T) {
	fs := toFields([]any{"a", 1, "dangling"})
	require.Len(t, fs, 2)
	assert.Equal(t, "a", fs[0].Key)
	assert.Equal(t, badKeyName, fs[1].Key, "落单的参数进 !BADKEY，不能静默吞掉")
}

// A call migrated from gfa's Sprintln semantics will hit this case.
func TestToFieldsNonStringKeyProducesBadKey(t *testing.T) {
	fs := toFields([]any{1001, 99.5}) // the kv portion of TInfo(ctx, "支付", id, amt)
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key)
	assert.Equal(t, badKeyName, fs[1].Key)
}

func TestToFieldsRealignsAfterBadKey(t *testing.T) {
	fs := toFields([]any{42, "user", "alice"})
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key, "42 当不了 key")
	assert.Equal(t, "user", fs[1].Key, "后面的合法 KV 必须重新对齐")
}

func TestFacadeCallerPointsToCallSite(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	L().Info("hi") // line + 1

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, line+1, e.Caller.Line, "caller 必须指向业务代码，不是 zap.go")
	assert.Contains(t, e.Caller.File, "zap_test.go")
}

// Zap() must return the instance without the facade's caller skip, otherwise
// the escape hatch's caller would be off by one frame.
func TestZapEscapeHatchReturnsRawLogger(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	z, ok := Zap(context.Background())
	require.True(t, ok)

	_, _, line, _ := runtime.Caller(0)
	z.Info("raw") // line + 1

	require.Len(t, logs.All(), 1)
	assert.Equal(t, line+1, logs.All()[0].Caller.Line)
}

func TestZapEscapeHatchFalseForNonZapBackend(t *testing.T) {
	SetLogger(Nop())
	t.Cleanup(func() { SetLogger(Nop()) })

	z, ok := Zap(context.Background())
	assert.False(t, ok)
	assert.Nil(t, z)
}

func TestWithCallerSkipCachesSkipOne(t *testing.T) {
	l := newZapLogger(zap.NewNop())
	a := l.WithCallerSkip(1)
	b := l.WithCallerSkip(1)
	assert.Same(t, a, b, "skip+1 是 T 系列热路径，必须缓存复用")
	assert.NotSame(t, a, l.WithCallerSkip(2))
}

func TestCtxFastPathReturnsStoredLogger(t *testing.T) {
	installObserver(t)
	stored := L().With("svc", "order")
	ctx := NewContext(context.Background(), stored)
	assert.Same(t, stored, Ctx(ctx), "存过 logger 就直接取，不再派生")
}

func TestCtxSlowPathDerivesFromTrace(t *testing.T) {
	logs := installObserver(t)

	tr := NewTrace("http.request")
	Ctx(WithTrace(context.Background(), tr)).Info("hit")

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, tr.TraceID().String(), m["trace_id"])
	assert.Equal(t, tr.SpanID().String(), m["span_id"])
	assert.Equal(t, tr.RequestID, m["request_id"])
	assert.Equal(t, "http.request", m["span_name"])
}

func TestCtxFallsBackToGlobal(t *testing.T) {
	installObserver(t)
	assert.NotPanics(t, func() {
		Ctx(context.Background()).Info("no trace")
		Ctx(nil).Info("nil ctx") //nolint:staticcheck
	})
}

func TestTraceKVSkipsZeroValues(t *testing.T) {
	kv := traceKV(Trace{})
	assert.Empty(t, kv, "零值 Trace 不该产出任何字段")
}

func TestEnabledReflectsCoreLevel(t *testing.T) {
	core, _ := observer.New(zapcore.WarnLevel)
	l := newZapLogger(zap.New(core))
	assert.False(t, l.Enabled(InfoLevel))
	assert.True(t, l.Enabled(WarnLevel))
	assert.True(t, l.Enabled(ErrorLevel))
}

func TestWithEmptyKVReturnsSameLogger(t *testing.T) {
	l := newZapLogger(zap.NewNop())
	assert.Same(t, Logger(l), l.With(), "空 With 不该白白克隆一个 logger")
}

// ── Init assembly ────────────────────────────────────────

func TestInitWithNoSinkYieldsNop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	assert.False(t, L().Enabled(ErrorLevel))
	assert.NotPanics(t, func() { L().Error("boom") })
}

func TestInitRejectsBadConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = "verbose"
	assert.Error(t, Init(cfg))
}

func TestInitFileSinkWritesJSONLWithMasking(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl") // extension implies json
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("下单", "order_id", 1001, "password", "hunter2")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &m))
	assert.Equal(t, "下单", m["msg"])
	assert.EqualValues(t, 1001, m["order_id"])
	assert.Equal(t, maskPlaceholder, m["password"], "脱敏必须覆盖文件 sink")
	assert.Contains(t, m, "caller")
	assert.Contains(t, m, "ts")
	assert.Equal(t, "info", m["level"])
}

func TestInitErrorPathOnlyReceivesErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	cfg.File.ErrorPath = filepath.Join(dir, "error.jsonl")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("普通信息")
	L().Error("出事了", "password", "hunter2")
	require.NoError(t, Close())

	all, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(all), "普通信息")
	assert.Contains(t, string(all), "出事了")

	errOnly, err := os.ReadFile(cfg.File.ErrorPath)
	require.NoError(t, err)
	assert.NotContains(t, string(errOnly), "普通信息", "error sink 只收 error 及以上")
	assert.Contains(t, string(errOnly), "出事了")

	// Every leaf sink is wrapped in its own maskCore layer -- missing any one of
	// them would be exposed right here.
	assert.NotContains(t, string(all), "hunter2", "app sink 必须脱敏")
	assert.NotContains(t, string(errOnly), "hunter2", "error sink 必须同样脱敏")
	assert.Contains(t, string(errOnly), maskPlaceholder)
}

// Regression test: the sampler must be wrapped outside maskCore (i.e., outside
// the Tee).
//
// If the order were reversed to newMaskCore(sampler), maskCore.Check would
// hang itself onto the CheckedEntry, and sampler.Check
// (zapcore/sampler.go:214-229 -- the only place sampling actually happens)
// would never run again, silently disabling log.sampling: all 5 duplicate log
// lines would be written to disk instead of just 1.
func TestSamplingWrapsOutsideMaskCore(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl")
	cfg.Sampling.Initial = 1
	cfg.Sampling.Thereafter = 0 // drop everything after the first in the window
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	for i := 0; i < 5; i++ {
		L().Info("重复消息")
	}
	require.NoError(t, Close())

	b, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, 1, bytes.Count(b, []byte("重复消息")),
		"采样必须生效：5 条相同消息只应落盘 1 条")
}

func TestInitCreatesMissingLogDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested")
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("x")
	require.NoError(t, Close())
	assert.FileExists(t, cfg.File.Path)
}

func TestSyncOnStdoutIsNotAnError(t *testing.T) {
	cfg := DefaultConfig() // console is on, points to stdout
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	assert.NoError(t, Sync(), "对终端/管道 fsync 会返回 EINVAL，那不是错误")
}

// ── SetLogger's safety warning ────────────────────────────

type fakeBackend struct{ Logger }

// captureStderr redirects os.Stderr to a pipe for the duration of fn,
// then returns whatever was written.
//
// This directly assigns os.Stderr = w, which is not concurrency-safe:
// tests using captureStderr must not call t.Parallel(). If a future test
// introduces parallelism, it will race on the os.Stderr global — the
// same constraint as nowFunc in rotate.go (see the comment there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stderr
	os.Stderr = w

	fn()

	os.Stderr = old
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func TestSetLoggerWarnsWhenBackendReplaced(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })

	out := captureStderr(t, func() { SetLogger(fakeBackend{Nop()}) })
	assert.Contains(t, out, "脱敏", "换后端等于换掉内置脱敏，必须显式警示")
}

func TestSetLoggerSilentForDefaultAndNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })

	out := captureStderr(t, func() {
		SetLogger(Nop())
		SetLogger(newZapLogger(zap.NewNop()))
	})
	assert.Empty(t, out, "默认 binding 与显式禁用都不该刷警告")
}

func TestSetLoggerNilFallsBackToNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })
	assert.NotPanics(t, func() {
		SetLogger(nil)
		L().Info("still fine")
	})
}
