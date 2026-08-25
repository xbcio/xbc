package xbc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// recordingLogger is a minimal log.Logger fake that records every call
// under a mutex. None of this package's existing test helpers fit: the log
// package's own fakes either discard everything (zap_test.go's fakeBackend)
// or bind to a real zap backend via zaptest/observer, which isn't visible
// outside the log package. Managed-goroutine tests only need to assert
// "an error was logged" and inspect its fields, so a small local recorder
// is simpler than reaching for either.
type logRecord struct {
	level log.Level
	msg   string
	kv    []any
}

type recordingLogger struct {
	mu      *sync.Mutex
	records *[]logRecord
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{mu: &sync.Mutex{}, records: &[]logRecord{}}
}

func (l *recordingLogger) record(lv log.Level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.records = append(*l.records, logRecord{level: lv, msg: msg, kv: kv})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record(log.DebugLevel, msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record(log.InfoLevel, msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record(log.WarnLevel, msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record(log.ErrorLevel, msg, kv) }

// Fatal is only present to satisfy log.Logger -- the brief's original
// six-method sketch predates this method landing on the interface. None of
// this task's code paths call Fatal, so it just records like the other
// levels instead of terminating the process (a fake that could kill the
// test binary would be worse than useless here).
func (l *recordingLogger) Fatal(msg string, kv ...any) { l.record(log.FatalLevel, msg, kv) }
func (l *recordingLogger) With(...any) log.Logger      { return l }
func (l *recordingLogger) Enabled(log.Level) bool      { return true }

func (l *recordingLogger) errorCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range *l.records {
		if r.level == log.ErrorLevel {
			n++
		}
	}
	return n
}

// newGoroutineTestApp builds a bare App with only the machinery Go/
// GoCritical touch -- cfg/registry/order etc. belong to other stages and
// are deliberately left nil, since this task's tests never reach them.
func newGoroutineTestApp(t *testing.T) *App {
	t.Helper()
	a := &App{}
	a.initGoroutines()
	return a
}

// newGoroutineTestContext wires a Context to a into a app for goroutine
// tests, with logger swapped for a recordingLogger so tests can assert on
// what got logged without touching the real log backend.
func newGoroutineTestContext(a *App, name string) (*Context, *recordingLogger) {
	rl := newRecordingLogger()
	ctx := &Context{app: a, name: name, instance: "default", logger: rl}
	return ctx, rl
}

// mustWaitTimeout bounds every blocking wait below. It is a failure-time
// upper bound, not a success-condition sleep: a passing test never waits
// for it, it only caps how long a real regression (a channel that should
// have closed, or a WaitGroup that should have drained, but didn't) takes
// to surface as a reported failure instead of hanging the test binary for
// the full "go test" timeout. That distinction matters because a batch of
// mutations against goManaged/triggerCritical (e.g. "the critical branch
// never fires", "triggerCritical never closes criticalCh") turns exactly
// the receives below into permanent blocks -- without a bound, running
// that batch costs an hour of wall clock instead of two seconds per
// mutation.
const mustWaitTimeout = 2 * time.Second

// mustClosed waits for ch to close (or to have a value ready), failing the
// test with msg if that does not happen within mustWaitTimeout.
func mustClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(mustWaitTimeout):
		t.Fatal(msg)
	}
}

// mustWait waits for wg to reach zero, failing the test with msg if that
// does not happen within mustWaitTimeout. wg.Wait itself takes no timeout,
// so this runs it on a helper goroutine and races that against the clock;
// if it times out, the helper goroutine is simply abandoned (calling
// wg.Wait, blocked) -- harmless leakage in a failing test that is about to
// tear the process down anyway.
func mustWait(t *testing.T, wg *sync.WaitGroup, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(mustWaitTimeout):
		t.Fatal(msg)
	}
}

// ---- Go: panic recovered but the application keeps running ----

func TestGoPanicRecoveredAppKeepsRunningAndLogsError(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "cron")

	ctx.Go(func(context.Context) {
		panic("模拟 cron 任务 panic")
	})

	mustWait(t, a.wg, "托管 goroutine panic 恢复后应该正常结束，wg.Wait 不应该被卡住")
	assert.Equal(t, 1, rl.errorCount(), "panic 必须被恢复并记一条 error 日志")
	select {
	case <-a.criticalCh:
		t.Fatal("Go 的 panic 不能触发 critical 关闭")
	default:
	}
	assert.Nil(t, a.runCtx.Err(), "Go 的 panic 不能取消 runCtx")
}

// ---- Go: a normal return is a non-event ----

func TestGoNormalReturnTriggersNothing(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "refresh")

	ctx.Go(func(context.Context) {})

	mustWait(t, a.wg, "正常返回的托管 goroutine 应该立即结束，wg.Wait 不应该被卡住")
	assert.Equal(t, 0, rl.errorCount(), "正常返回不应该记任何 error 日志")
	select {
	case <-a.criticalCh:
		t.Fatal("Go 正常返回不能触发 critical 关闭")
	default:
	}
}

// ---- Go: cancel arrives during shutdown, and is waited on by the WaitGroup ----

func TestGoObservesShutdownCancelAndIsWaitedOn(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, _ := newGoroutineTestContext(a, "consumer")

	sawDone := make(chan struct{})
	ctx.Go(func(c context.Context) {
		<-c.Done()
		close(sawDone)
	})

	a.cancel()
	mustClosed(t, sawDone, "托管 goroutine 必须真的观察到 cancel 信号并返回，不能是恰好碰到别的原因退出")

	mustWait(t, a.wg, "wg.Wait 应该在托管 goroutine 观察到 cancel 并返回后完成")
}

// ---- GoCritical: panic triggers shutdown ----

func TestGoCriticalPanicTriggersShutdown(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "consumer")

	ctx.GoCritical(func(context.Context) {
		panic("模拟 consumer panic")
	})

	mustClosed(t, a.criticalCh, "GoCritical 的 panic 必须触发 triggerCritical，criticalCh 必须被关闭")
	mustWait(t, a.wg, "触发 critical 之后，托管 goroutine 仍然要正常结束，wg.Wait 不应该被卡住")
	assert.Equal(t, 1, rl.errorCount(), "panic 必须被恢复并记一条 error 日志")
	assert.NotEmpty(t, a.criticalReason, "触发原因必须被记录")
	require.Error(t, a.runCtx.Err(), "触发 critical 必须取消 runCtx，这正是 stage_run.go 的 shutdown 依赖的信号")
}

// ---- GoCritical: an early normal return also triggers shutdown ----

func TestGoCriticalEarlyReturnTriggersShutdown(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "gateway")

	ctx.GoCritical(func(context.Context) {})

	mustClosed(t, a.criticalCh, "GoCritical 的提前正常返回必须触发 triggerCritical，criticalCh 必须被关闭")
	mustWait(t, a.wg, "触发 critical 之后，托管 goroutine 仍然要正常结束，wg.Wait 不应该被卡住")
	assert.Equal(t, 0, rl.errorCount(), "提前正常返回不是 panic，不应该记 error 日志，但仍要触发关闭")
	assert.Contains(t, a.criticalReason, "意外提前返回")
	require.Error(t, a.runCtx.Err())
}

// ---- GoCritical: a return caused by a clean (non-critical) shutdown must
// not itself fire triggerCritical ----
//
// TestGoCriticalReturnDuringShutdownDoesNotRetrigger above only exercises
// the case where runCtx was already cancelled by triggerCritical itself
// (consumer-a's panic). That leaves a gap: if goManaged computed "was this
// return expected" as unconditionally false instead of checking
// a.runCtx.Err(), that mutant would still pass every test above --
// consumer-a's panic has already latched criticalOnce, so a second,
// incorrect triggerCritical call from consumer-b is swallowed silently and
// invisible to assertions on a.criticalReason. The gap only shows up when
// runCtx is cancelled by something other than triggerCritical (e.g. a
// clean SIGTERM-driven shutdown calling a.cancel() directly) and there is
// no prior critical to hide behind. This test closes that gap directly.
func TestGoCriticalReturnAfterCleanCancelDoesNotTrigger(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "consumer")

	started := make(chan struct{})
	ctx.GoCritical(func(c context.Context) {
		close(started)
		<-c.Done() // returns once a.cancel() below fires, same as a clean shutdown would
	})
	mustClosed(t, started, "consumer 的托管 goroutine 必须先跑起来才能观察后续的 cancel")

	a.cancel() // simulates stage 10's clean shutdown cancelling runCtx directly, NOT via triggerCritical
	mustWait(t, a.wg, "干净关闭引发的返回也要被正常等到，wg.Wait 不应该被卡住")

	assert.Equal(t, 0, rl.errorCount(), "干净关闭期间的正常返回不是 panic，不应该记 error 日志")
	select {
	case <-a.criticalCh:
		t.Fatal("干净关闭引发的返回不能被误判成 GoCritical 的意外提前返回，criticalCh 不该被关闭")
	default:
	}
	assert.Empty(t, a.criticalReason, "没有发生 critical，criticalReason 必须保持空")
}

// ---- GoCritical: returning during shutdown does not re-trigger ----

func TestGoCriticalReturnDuringShutdownDoesNotRetrigger(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctxA, _ := newGoroutineTestContext(a, "consumer-a")
	ctxB, rlB := newGoroutineTestContext(a, "consumer-b")

	started := make(chan struct{})
	ctxB.GoCritical(func(c context.Context) {
		close(started)
		<-c.Done() // returns only once consumer-a's panic cancels runCtx
	})
	mustClosed(t, started, "consumer-b 的托管 goroutine 必须先跑起来才能观察后续的 cancel")

	ctxA.GoCritical(func(context.Context) {
		panic("模拟 consumer-a panic")
	})

	mustWait(t, a.wg, "两个托管 goroutine 都必须正常结束：不能死锁，也不能因为第二次 close 而 panic")
	assert.Equal(t, "插件 consumer-a 的托管 goroutine panic: 模拟 consumer-a panic", a.criticalReason,
		"只保留第一个触发原因，consumer-b 的返回不能覆盖它")
	assert.Equal(t, 0, rlB.errorCount(), "consumer-b 是响应 shutdown 的正常返回，不是 panic，不应该记 error 日志")
}

// ---- triggerCritical's signal contract: stage_run.go relies on it for exit code 1 ----
//
// This task sits before stage_init.go / stage_run.go in execution order, and
// the repo does not yet have a complete testable run(), so verification only
// goes as far as "the signal was correctly lit": criticalCh closed, runCtx
// cancelled. The code that actually reads those two signals, drives the full
// stage 10, and sets a.exitCode to 1 is stage_run.go's serve()/shutdown()
// (Task 14's TestGoCriticalTriggersFullShutdownAndExitCodeOne covers that
// part, except its test hand-writes close(a.criticalCh) to simulate the
// trigger, without actually calling triggerCritical).
func TestTriggerCriticalSignalIsWhatStage9ReadsForExitCodeOne(t *testing.T) {
	a := newGoroutineTestApp(t)
	a.triggerCritical("手工触发，验证契约")

	_, stillOpen := <-a.criticalCh
	assert.False(t, stillOpen, "criticalCh 必须已关闭——stage_run.go 的 serve() 在 select 里等的正是这个信号")
	require.Error(t, a.runCtx.Err(),
		"runCtx 必须已取消——真正把 exitCode 置 1 的是 stage_run.go 的 shutdown(\"critical\")，本 task 只负责点燃信号")
}

// ---- Concurrent criticals: keep only the first reason, and never block each other ----

func TestTriggerCriticalConcurrentFailuresKeepOnlyFirstReasonAndDoNotBlock(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx1, rl1 := newGoroutineTestContext(a, "worker-1")
	ctx2, rl2 := newGoroutineTestContext(a, "worker-2")

	release := make(chan struct{})
	ctx1.GoCritical(func(context.Context) {
		<-release
		panic("worker-1 炸了")
	})
	ctx2.GoCritical(func(context.Context) {
		<-release
		panic("worker-2 炸了")
	})
	close(release) // makes the two panics happen as close to simultaneously as possible

	mustWait(t, a.wg, "并发 critical 触发不能让任何一个托管 goroutine 卡死")

	assert.Equal(t, 1, rl1.errorCount())
	assert.Equal(t, 1, rl2.errorCount(), "各自的 panic 仍然各自记一条 error 日志，触发 shutdown 这个动作才只认第一个")
	assert.True(t,
		a.criticalReason == "插件 worker-1 的托管 goroutine panic: worker-1 炸了" ||
			a.criticalReason == "插件 worker-2 的托管 goroutine panic: worker-2 炸了",
		"必须恰好保留其中一个原因，不能是空、也不能是两个拼接在一起")
}

// ---- The WaitGroup really does wait for every managed goroutine to return before returning itself ----

func TestWaitGroupBlocksUntilAllManagedGoroutinesReturn(t *testing.T) {
	a := newGoroutineTestApp(t)
	const n = 5
	release := make(chan struct{})
	var mu sync.Mutex
	var finished []int

	for i := 0; i < n; i++ {
		ctx, _ := newGoroutineTestContext(a, "worker")
		i := i
		ctx.Go(func(context.Context) {
			<-release
			mu.Lock()
			finished = append(finished, i)
			mu.Unlock()
		})
	}

	close(release)
	mustWait(t, a.wg, "释放全部 goroutine 后 wg.Wait 必须返回")

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, finished, n, "wg.Wait 返回时必须是全部托管 goroutine 都已经跑完，一个都不能少")
}
