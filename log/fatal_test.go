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

	l.Fatal("数据库连接失败", "attempt", 3)

	assert.Equal(t, 1, code, "Fatal 必须以退出码 1 结束进程")
	// zapcore's ioCore syncs on its own for levels above Error, so "sync" can
	// legitimately appear more than once. What must hold is the shape: the
	// write comes first, the exit comes last, and a flush sits between them.
	require.GreaterOrEqual(t, len(ws.events), 3)
	assert.Equal(t, "write", ws.events[0], "必须先写入")
	assert.Equal(t, "exit", ws.events[len(ws.events)-1], "退出必须是最后一步")
	assert.Contains(t, ws.events[1:len(ws.events)-1], "sync",
		"写与退出之间必须落盘——顺序错了就等于丢掉最该保留的那条日志")

	out := ws.buf.String()
	assert.Contains(t, out, "数据库连接失败")
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

	l.Fatal("退出一次")

	assert.Equal(t, 1, calls, "退出只能发生一次，且必须由门面发起")
	assert.Contains(t, ws.events, "sync", "zap 自带的 fatal hook 会跳过 Sync")
}

// A Sync failure must not swallow the exit -- the process still has to die.
func TestFatalExitsEvenWhenSyncFails(t *testing.T) {
	ws := &recordingSyncer{syncEr: assert.AnError}
	l := fatalTestLogger(ws)

	code := -1
	stubExit(t, func(c int) { code = c })

	l.Fatal("落盘会失败，但仍要退出")
	assert.Equal(t, 1, code, "Sync 失败也必须退出")
}

// Nop drops the message -- that is what Nop means -- but it must still honour
// the termination half of Fatal's contract. A caller writing log.Fatal expects
// the next line never to run; silently continuing turns a facade choice into a
// control-flow bug in the caller. zap made the same call: zap.NewNop().Fatal
// exits too.
func TestNopFatalStillExits(t *testing.T) {
	code := -1
	stubExit(t, func(c int) { code = c })

	Nop().Fatal("即使是 Nop 也要退出", "k", "v")
	assert.Equal(t, 1, code, "Nop().Fatal 必须仍然退出进程")
}

func TestTFatalGoesThroughTheSameExit(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	ctx := NewContext(context.Background(), l)

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatal(ctx, "语法糖也要退出", "order_id", 1001)

	assert.Equal(t, 1, code)
	assert.Contains(t, ws.buf.String(), "语法糖也要退出")
	assert.Contains(t, ws.buf.String(), `"order_id":1001`)
}

func TestTFatalfGoesThroughTheSameExit(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	ctx := NewContext(context.Background(), l)

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatalf(ctx, "订单 %d 无法恢复", 1001)

	assert.Equal(t, 1, code)
	assert.Contains(t, ws.buf.String(), "订单 1001 无法恢复")
}

// TFatalf must not guard on Enabled: a disabled backend reports false, and a
// guarded call would fall through, letting the caller run past a line written
// on the assumption it never returns.
func TestTFatalfExitsEvenWhenLevelDisabled(t *testing.T) {
	ctx := NewContext(context.Background(), Nop())
	require.False(t, Nop().Enabled(FatalLevel), "前提：Nop 报告所有级别都禁用")

	code := -1
	stubExit(t, func(c int) { code = c })

	TFatalf(ctx, "级别禁用也要退出 %d", 1001)
	assert.Equal(t, 1, code, "TFatalf 不能因为级别禁用就跳过退出")
}

// ── Level plumbing ──────────────────────────────────────────────────────

// FatalLevel must be 5, not 3: the numeric values mirror zapcore.Level, where
// 3 and 4 are DPanic and Panic. Writing it as the next iota would silently map
// Fatal onto DPanic and every Fatal entry would render as DPANIC.
func TestFatalLevelMirrorsZapcore(t *testing.T) {
	assert.Equal(t, Level(5), FatalLevel)
	assert.Equal(t, zapcore.FatalLevel, zapcore.Level(FatalLevel),
		"门面级别与 zapcore 的数值必须一一对应")
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
	assert.True(t, l.Enabled(FatalLevel), "即使级别配到 error，Fatal 仍应输出")
	assert.False(t, l.Enabled(InfoLevel))
}

// The escape hatch keeps zap's native semantics, so it must NOT carry the
// no-op fatal hook the facade installs on its own instance.
func TestZapEscapeHatchKeepsNativeFatal(t *testing.T) {
	ws := &recordingSyncer{}
	l := fatalTestLogger(ws)
	assert.NotSame(t, l.Zap(), l.z, "逃生舱返回的必须是未加 hook 的原始实例")
}
