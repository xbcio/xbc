package log

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The T-series adds one more stack frame than the facade methods; the caller
// must still point to the calling business code.
// gfa's single AddCallerSkip(2) would point to the wrong frame when going
// through the facade methods -- both paths are tested here.
func TestTSeriesCallerPointsToCallSite(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	TInfo(context.Background(), "via T")         // line + 1
	L().Info("via facade")                       // line + 2
	TInfof(context.Background(), "via %s", "Tf") // line + 3

	require.Len(t, logs.All(), 3)
	assert.Equal(t, line+1, logs.All()[0].Caller.Line, "TInfo 的 caller")
	assert.Equal(t, line+2, logs.All()[1].Caller.Line, "门面方法的 caller")
	assert.Equal(t, line+3, logs.All()[2].Caller.Line, "TInfof 的 caller")

	for _, e := range logs.All() {
		assert.Contains(t, e.Caller.File, "sugar_test.go")
	}
}

func TestTSeriesLevels(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TDebug(ctx, "d")
	TInfo(ctx, "i")
	TWarn(ctx, "w")
	TError(ctx, "e")

	got := make([]zapcore.Level, 0, 4)
	for _, e := range logs.All() {
		got = append(got, e.Level)
	}
	assert.Equal(t, []zapcore.Level{
		zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel, zapcore.ErrorLevel,
	}, got)
}

func TestTSeriesCarriesKV(t *testing.T) {
	logs := installObserver(t)
	TInfo(context.Background(), "下单", "order_id", 1001, "amount", 99.5)

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.EqualValues(t, 1001, m["order_id"])
	assert.InDelta(t, 99.5, m["amount"], 0.001)
}

func TestTSeriesEquivalentToFacade(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TInfo(ctx, "同一条", "k", "v")
	Ctx(ctx).Info("同一条", "k", "v")

	require.Len(t, logs.All(), 2)
	assert.Equal(t, logs.All()[0].Message, logs.All()[1].Message)
	assert.Equal(t, logs.All()[0].ContextMap(), logs.All()[1].ContextMap(),
		"两条路径必须产出完全相同的日志")
}

func TestTSeriesInheritsContextTrace(t *testing.T) {
	logs := installObserver(t)

	tr := NewTrace("http.request")
	TInfo(WithTrace(context.Background(), tr), "带链路")

	require.Len(t, logs.All(), 1)
	assert.Equal(t, tr.TraceID().String(), logs.All()[0].ContextMap()["trace_id"])
}

func TestTSeriesFormatVariants(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TDebugf(ctx, "conn=%d", 10)
	TInfof(ctx, "user=%s age=%d", "alice", 30)
	TWarnf(ctx, "retry %d/%d", 2, 3)
	TErrorf(ctx, "failed: %v", context.Canceled)

	require.Len(t, logs.All(), 4)
	assert.Equal(t, "conn=10", logs.All()[0].Message)
	assert.Equal(t, "user=alice age=30", logs.All()[1].Message)
	assert.Equal(t, "retry 2/3", logs.All()[2].Message)
	assert.Equal(t, "failed: context canceled", logs.All()[3].Message)
}

type stringerFunc func() string

func (f stringerFunc) String() string { return f() }

// The f version must check the level before formatting, otherwise a disabled
// log level still pays the formatting cost.
func TestTInfofSkipsFormattingWhenLevelDisabled(t *testing.T) {
	core, _ := observer.New(zapcore.ErrorLevel) // info not enabled
	SetLogger(newZapLogger(zap.New(core)))
	t.Cleanup(func() { SetLogger(Nop()) })

	calls := 0
	arg := stringerFunc(func() string { calls++; return "expensive" })

	TInfof(context.Background(), "%s", arg)
	assert.Zero(t, calls, "级别不启用时不该触发格式化")

	TErrorf(context.Background(), "%s", arg)
	assert.Equal(t, 1, calls, "启用的级别照常格式化")
}

func TestTSeriesNilContextDoesNotPanic(t *testing.T) {
	installObserver(t)
	assert.NotPanics(t, func() {
		TInfo(nil, "nil ctx")        //nolint:staticcheck
		TInfof(nil, "nil ctx %d", 1) //nolint:staticcheck
	})
}

func TestTSeriesWorksWithBackendLackingCallerSkipper(t *testing.T) {
	SetLogger(Nop()) // nopLogger doesn't implement CallerSkipper
	t.Cleanup(func() { SetLogger(Nop()) })

	assert.NotPanics(t, func() {
		TInfo(context.Background(), "后端不支持 skip 也要能打")
		TInfof(context.Background(), "%d", 1)
	})
}
