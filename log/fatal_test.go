package log

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Fatal is the one facade method that must not let zap own the exit: zap's own
// Fatal calls os.Exit from inside the write, so anything the sink still holds
// dies with the process -- and the entry explaining why the process died is the
// one most worth keeping. The ordering assertions below are the whole point of
// the method existing.

// recordingSyncer records the order of Write and Sync so a test can prove the
// flush happened before the exit, not after.
type recordingSyncer struct {
	buf    bytes.Buffer
	events []string
	syncEr error
}

func (s *recordingSyncer) Write(p []byte) (int, error) {
	s.events = append(s.events, "write")
	return s.buf.Write(p)
}

func (s *recordingSyncer) Sync() error {
	s.events = append(s.events, "sync")
	return s.syncEr
}

// stubExit swaps exitFunc for the duration of a test and returns the recorded
// exit codes plus a restore func.
func stubExit(t *testing.T, rec func(int)) {
	t.Helper()
	old := exitFunc
	exitFunc = rec
	t.Cleanup(func() { exitFunc = old })
}

func fatalTestLogger(ws zapcore.WriteSyncer) *zapLogger {
	return newZapLogger(zap.New(
		zapcore.NewCore(zapcore.NewJSONEncoder(jsonEncoderConfig()), ws, zapcore.DebugLevel),
	))
}

func TestFatalWritesThenSyncsThenExits(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)

	code := -1
	stubExit(t, func(c int) {
		code = c
		ws.events = append(ws.events, "exit")
	})

	l.Fatal("Database connection failed", "attempt", 3)

	assert.Equal(t, 1, code, "Fatal must exit with exit code 1")
	// zapcore's ioCore syncs on its own for levels above Error, so "sync" can
	// legitimately appear more than once. What must hold is the shape: the
	// write comes first, the exit comes last, and a flush sits between them.
	require.GreaterOrEqual(t, len(ws.events), 3)
	assert.Equal(t, "write", ws.events[0], "Must write first")
	assert.Equal(t, "exit", ws.events[len(ws.events)-1], "Exit must be the last step")
	assert.Contains(t, ws.events[1:len(ws.events)-1], "sync",
		"Writing and exit must flush to disk—order wrong equals losing the most critical log")

	out := ws.buf.String()
	assert.Contains(t, out, "Database connection failed")
	assert.Contains(t, out, `"level":"fatal"`)
	assert.Contains(t, out, `"attempt":3`)
}

// If zap kept the default fatal hook, the entry would exit inside Write and
// neither the Sync nor our own exitFunc would ever run.
func TestFatalDoesNotLetZapOwnTheExit(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)

	calls := 0
	stubExit(t, func(int) { calls++ })

	l.Fatal("Exit once")

	assert.Equal(t, 1, calls, "Exit can only happen once, and must be initiated by the facade")
	assert.Contains(t, ws.events, "sync", "zap's built-in fatal hook skips Sync")
}

// A Sync failure must not swallow the exit -- the process still has to die.
func TestFatalExitsEvenWhenSyncFails(t *testing.T) {
	ws := &recordingSyncer{syncEr: assert.AnError}
	l := fatalTestLogger(ws)

	code := -1
	stubExit(t, func(c int) { code = c })

	l.Fatal("Flush will fail, but exit still required")
	assert.Equal(t, 1, code, "Sync failure must still exit")
}

// Nop drops the message -- that is what Nop means -- but it must still honour
// the termination half of Fatal's contract. A caller writing log.Fatal expects
// the next line never to run; silently continuing turns a facade choice into a
// control-flow bug in the caller. zap made the same call: zap.NewNop().Fatal
// exits too.
func TestNopFatalStillExits(t *testing.T) {
	code := -1
	stubExit(t, func(c int) { code = c })

	Nop().Fatal("Even Nop must exit", "k", "v")
	assert.Equal(t, 1, code, "Nop().Fatal must still exit process")
}

func TestTFatalGoesThroughTheSameExit(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	ctx := NewContext(context.Background(), l)

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatal(ctx, "Syntactic sugar must also exit", "order_id", 1001)

	assert.Equal(t, 1, code)
	assert.Contains(t, ws.buf.String(), "Syntactic sugar must also exit")
	assert.Contains(t, ws.buf.String(), `"order_id":1001`)
}

func TestTFatalfGoesThroughTheSameExit(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	ctx := NewContext(context.Background(), l)

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatalf(ctx, "Order %d cannot be recovered", 1001)

	assert.Equal(t, 1, code)
	assert.Contains(t, ws.buf.String(), "Order 1001 cannot be recovered")
}

// TFatalf must not guard on Enabled: a disabled backend reports false, and a
// guarded call would fall through, letting the caller run past a line written
// on the assumption it never returns.
func TestTFatalfExitsEvenWhenLevelDisabled(t *testing.T) {
	ctx := NewContext(context.Background(), Nop())
	require.False(t, Nop().Enabled(FatalLevel), "Prerequisite: Nop disables all log levels")

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatalf(ctx, "Disabled level must also exit %d", 1001)
	assert.Equal(t, 1, code, "TFatalf cannot skip exit due to disabled level")
}

// ── Level plumbing ──────────────────────────────────────────────────────

// FatalLevel must be 5, not 3: the numeric values mirror zapcore.Level, where
// 3 and 4 are DPanic and Panic. Writing it as the next iota would silently map
// Fatal onto DPanic and every Fatal entry would render as DPANIC.
func TestFatalLevelMirrorsZapcore(t *testing.T) {
	assert.Equal(t, Level(5), FatalLevel)
	assert.Equal(t, zapcore.FatalLevel, zapcore.Level(FatalLevel),
		"Facade level must match zapcore value exactly")
	assert.Equal(t, "FATAL", FatalLevel.String())
}

func TestParseLevelAcceptsFatal(t *testing.T) {
	lv, err := ParseLevel("fatal")
	require.NoError(t, err)
	assert.Equal(t, FatalLevel, lv)

	lv, err = ParseLevel("FATAL")
	require.NoError(t, err)
	assert.Equal(t, FatalLevel, lv)
}

// Fatal is the highest level, so it can never be filtered out by config.
func TestFatalAlwaysEnabled(t *testing.T) {
	ws := &recordingSyncer{}
	l := newZapLogger(zap.New(
		zapcore.NewCore(zapcore.NewJSONEncoder(jsonEncoderConfig()), ws, zapcore.ErrorLevel),
	))
	assert.True(t, l.Enabled(FatalLevel), "Even if level is set to error, Fatal must still output")
	assert.False(t, l.Enabled(InfoLevel))
}

// The escape hatch keeps zap's native semantics, so it must NOT carry the
// no-op fatal hook the facade installs on its own instance.
func TestZapEscapeHatchKeepsNativeFatal(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	assert.NotSame(t, l.Zap(), l.z, "Escape pod must return original instance without hook")
}
