package log

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

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
	assert.Equal(t, badKeyName, fs[1].Key, "Single parameter goes to !BADKEY, cannot be silently dropped")
}

// A call migrated from gfa's Sprintln semantics will hit this case.
func TestToFieldsNonStringKeyProducesBadKey(t *testing.T) {
	fs := toFields([]any{1001, 99.5}) // the kv portion of TInfo(ctx, "payment", id, amt)
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key)
	assert.Equal(t, badKeyName, fs[1].Key)
}

func TestToFieldsRealignsAfterBadKey(t *testing.T) {
	fs := toFields([]any{42, "user", "alice"})
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key, "42 cannot be a key")
	assert.Equal(t, "user", fs[1].Key, "Subsequent valid KV must realign")
}

func TestFacadeCallerPointsToCallSite(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	L().Info("hi") // line + 1

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, line+1, e.Caller.Line, "caller must point to business code, not zap.go")
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
	assert.Same(t, a, b, "skip+1 is the T series hot path, must be cached and reused")
	assert.NotSame(t, a, l.WithCallerSkip(2))
}

func TestCtxFastPathReturnsStoredLogger(t *testing.T) {
	installObserver(t)
	stored := L().With("svc", "order")
	ctx := NewContext(context.Background(), stored)
	assert.Same(t, stored, Ctx(ctx), "Take the stored logger directly, no longer derive")
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
	assert.Empty(t, kv, "Zero-value Trace should not produce any fields")
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
	assert.Same(t, Logger(l), l.With(), "Empty With should not clone a logger unnecessarily")
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

	L().Info("Place an order", "order_id", 1001, "password", "hunter2")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &m))
	assert.Equal(t, "Place an order", m["msg"])
	assert.EqualValues(t, 1001, m["order_id"])
	assert.Equal(t, maskPlaceholder, m["password"], "Sensitive fields must be masked in the file sink")
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

	L().Info("Normal information")
	L().Error("Something went wrong", "password", "hunter2")
	require.NoError(t, Close())

	all, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(all), "Normal information")
	assert.Contains(t, string(all), "Something went wrong")

	errOnly, err := os.ReadFile(cfg.File.ErrorPath)
	require.NoError(t, err)
	assert.NotContains(t, string(errOnly), "Normal information", "error sink only accepts error and above")
	assert.Contains(t, string(errOnly), "Something went wrong")

	// Every leaf sink is wrapped in its own maskCore layer -- missing any one of
	// them would be exposed right here.
	assert.NotContains(t, string(all), "hunter2", "app sink must redact sensitive fields")
	assert.NotContains(t, string(errOnly), "hunter2", "error sink must also redact sensitive fields")
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
		L().Info("Duplicate message")
	}
	require.NoError(t, Close())

	b, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, 1, bytes.Count(b, []byte("Duplicate message")),
		"Sampling must take effect: 5 identical messages should only be logged once")
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

	assert.NoError(t, Sync(), "Fsync to terminal/pipeline fs will return EINVAL, which is not an error")
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
	assert.Contains(t, out, "sensitive field masking", "Replacing the backend disables built-in sensitive field masking, so the warning must be explicit")
}

func TestSetLoggerSilentForDefaultAndNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })

	out := captureStderr(t, func() {
		SetLogger(Nop())
		SetLogger(newZapLogger(zap.NewNop()))
	})
	assert.Empty(t, out, "Default binding and explicit disable should not log warnings")
}

func TestSetLoggerNilFallsBackToNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })
	assert.NotPanics(t, func() {
		SetLogger(nil)
		L().Info("still fine")
	})
}

// ── Lazy-closing sink ──────────────────────────────────────

func TestLazyClosingSinkWriteErrorsAfterClose(t *testing.T) {
	buf := zapcore.AddSync(&bytes.Buffer{})
	closed := false
	s := newLazyClosingSink(buf, func() error { closed = true; return nil })

	n, err := s.Write([]byte("before close"))
	require.NoError(t, err)
	assert.Equal(t, len("before close"), n)

	require.NoError(t, s.Close())
	assert.True(t, closed)

	n, err = s.Write([]byte("after close"))
	assert.Error(t, err, "a write after Close must be reported, not silently dropped or delegated")
	assert.Equal(t, 0, n)
}

func TestLazyClosingSinkSyncIsNoopAfterClose(t *testing.T) {
	s := newLazyClosingSink(zapcore.AddSync(&bytes.Buffer{}), func() error { return nil })
	require.NoError(t, s.Close())
	assert.NoError(t, s.Sync(), "Sync on a closed sink must be harmless")
}

func TestLazyClosingSinkCloseIsIdempotent(t *testing.T) {
	calls := 0
	s := newLazyClosingSink(zapcore.AddSync(&bytes.Buffer{}), func() error { calls++; return nil })
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	assert.Equal(t, 1, calls, "the underlying close func must run exactly once")
}

// gatedSyncer blocks inside Write until release is closed, so a test can hold
// a write in flight and observe what Close does while it is there.
type gatedSyncer struct {
	entered chan struct{}
	release chan struct{}
	inWrite atomic.Bool
}

func (g *gatedSyncer) Write(p []byte) (int, error) {
	g.inWrite.Store(true)
	close(g.entered)
	<-g.release
	g.inWrite.Store(false)
	return len(p), nil
}

func (g *gatedSyncer) Sync() error { return nil }

// TestLazyClosingSinkCloseWaitsForInFlightWrite pins the ordering the closed
// check alone cannot give: checking closed and then delegating leaves a window
// in which Close can complete and the delegate then lands on a closed sink --
// for a lumberjack sink that is the reopen-on-write resurrection again. A
// write that passed the check must therefore keep the sink open until it
// finishes delegating, and Close must not run the real close func while that
// delegation is still in flight.
func TestLazyClosingSinkCloseWaitsForInFlightWrite(t *testing.T) {
	syncer := &gatedSyncer{entered: make(chan struct{}), release: make(chan struct{})}
	var closedDuringWrite atomic.Bool
	s := newLazyClosingSink(syncer, func() error {
		if syncer.inWrite.Load() {
			closedDuringWrite.Store(true)
		}
		return nil
	})

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = s.Write([]byte("in flight"))
	}()
	<-syncer.entered

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = s.Close()
	}()

	select {
	case <-closeDone:
		t.Fatal("Close completed while a write was still delegating to the sink it closes")
	case <-time.After(50 * time.Millisecond):
	}

	close(syncer.release)
	<-writeDone
	<-closeDone

	assert.False(t, closedDuringWrite.Load(),
		"the underlying close must not run concurrently with a write it would pull the sink from under")
	_, err := s.Write([]byte("after close"))
	assert.ErrorIs(t, err, errSinkClosed,
		"a write after Close must report the package's discard error, not a freshly allocated one")
}

// ── Close / Init must not let a cached Logger resurrect a closed file ──────

// Regression test for the audit finding: a caller holding an earlier L()
// result that keeps writing after Close() must not make lumberjack silently
// reopen the file Close just closed. Before the fix, Close closed the sink
// while the global logger still pointed at it, and lumberjack.Write's
// `if l.file == nil { openExistingOrNew }` resurrected the file on the very
// next write through the cached Logger.
func TestCachedLoggerCannotResurrectFileAfterClose(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	cached := L() // simulates a caller that stashed an earlier L() result
	cached.Info("line one")
	require.NoError(t, Close())

	info, err := os.Stat(cfg.File.Path)
	require.NoError(t, err)
	sizeAtClose := info.Size()
	assert.Positive(t, sizeAtClose)

	// Writes through the cached value after Close must not grow the file.
	for i := 0; i < 5; i++ {
		cached.Info("line after close")
	}

	info, err = os.Stat(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, sizeAtClose, info.Size(),
		"writing through a Logger cached before Close must not reopen or grow the closed file")
}

// Regression test for the same finding, but across a second Init rather than
// Close: Init must swap the global logger before closing the previous
// round's sinks, otherwise a concurrent write landing on the old cached
// Logger during the handover window hits an already-closed lumberjack and
// resurrects the old file.
func TestCachedLoggerCannotResurrectFileAcrossReinit(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "first.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	oldCached := L()
	oldCached.Info("first round")

	cfg2 := cfg
	cfg2.File.Path = filepath.Join(dir, "second.log")
	require.NoError(t, Init(cfg2))

	oldInfo, err := os.Stat(cfg.File.Path)
	require.NoError(t, err)
	oldSizeAfterReinit := oldInfo.Size()

	for i := 0; i < 5; i++ {
		oldCached.Info("stale write")
	}
	L().Info("new round") // through the new global, must land in second.log

	oldInfoAfter, err := os.Stat(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, oldSizeAfterReinit, oldInfoAfter.Size(),
		"writes through the pre-reinit cached Logger must not grow the old file")

	require.NoError(t, Close())
	newData, err := os.ReadFile(cfg2.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(newData), "new round")
}

// Two consecutive Init calls at different paths: only the new file grows,
// the old one is untouched from that point on.
func TestConsecutiveInitOnlyNewFileGrows(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "a.log")
	require.NoError(t, Init(cfg))

	L().Info("into a")
	require.NoError(t, Sync())
	aInfo, err := os.Stat(cfg.File.Path)
	require.NoError(t, err)
	aSize := aInfo.Size()
	assert.Positive(t, aSize)

	cfg2 := cfg
	cfg2.File.Path = filepath.Join(dir, "b.log")
	require.NoError(t, Init(cfg2))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("into b")
	require.NoError(t, Sync())

	aInfoAfter, err := os.Stat(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, aSize, aInfoAfter.Size(), "the old file must not grow after a second Init")

	bInfo, err := os.Stat(cfg2.File.Path)
	require.NoError(t, err)
	assert.Positive(t, bInfo.Size())
}

// Deleting the closed file after Close and writing through the cached
// Logger again must not recreate it -- the strongest form of "does not
// resurrect": not just "does not grow", but "does not even come back into
// existence".
func TestCachedLoggerDoesNotRecreateDeletedFileAfterClose(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	cached := L()
	cached.Info("line one")
	require.NoError(t, Close())
	require.NoError(t, os.Remove(cfg.File.Path))

	cached.Info("should not recreate the file")

	_, err := os.Stat(cfg.File.Path)
	assert.True(t, os.IsNotExist(err), "a write through a cached Logger after Close must not recreate the deleted file")
}

// After Close, the facade's own global falls back to discarding -- not to
// a zapLogger still pointed at closed sinks.
func TestCloseSwapsOwnBackendToNop(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	require.NoError(t, Close())
	assert.Equal(t, Nop(), L(), "Close must swap its own backend to Nop so L() stops pointing at closed sinks")
}

// Close must not clobber a third-party backend installed after Init via
// SetLogger -- it only owns the backend it, through Init, put there itself.
func TestCloseLeavesThirdPartyBackendAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	third := fakeBackend{Nop()}
	SetLogger(third)

	require.NoError(t, Close())
	assert.Equal(t, Logger(third), L(), "Close must not replace a third-party backend installed via SetLogger")
}
