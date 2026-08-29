package runtime

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// signalSelf sends sig to the test binary's own process.
//
// It never touches *testing.T: it is called from goroutines other than the
// one running the test (a plugin hook executing inside App.Execute, itself
// running inside runAsync's goroutine), and testing.T's Fatal/FailNow family
// must only be called from the goroutine that is running the test function.
// Errors are handed back to the caller to assert on from the right
// goroutine instead.
func signalSelf(sig syscall.Signal) error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// signalWaitUntil polls cond until it holds or timeout elapses, returning
// whether it held.
//
// This is waitFor's logic without the *testing.T dependency, for exactly the
// same reason signalSelf has none: it has to run safely from a goroutine
// that is not the one running the test. The sleep is strictly an upper bound
// on how long a failing case is allowed to take -- success is always
// observed by cond() returning true, never by the sleep itself.
func signalWaitUntil(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Microsecond)
	}
	return false
}

// TestSignalRegisteredBeforeFirstInit pins the ordering rule spelled out in
// watchProcessSignals' own doc comment: signal.Notify must already be in effect
// before the very first user Init runs, not merely before the run loop
// blocks. A framework that registered the handler any later would let a
// SIGTERM arriving while the first plugin's Init is still in flight fall
// through to the Go runtime's default disposition and kill the process
// outright -- nothing already Init'd would ever see Stop.
//
// The pin: plugin "signal-first" sends SIGTERM to the test process's own
// pid from inside its Init, then polls (bounded, never a sleep used as the
// success condition -- see signalWaitUntil) until the App has actually
// observed the stop request before returning. If watchProcessSignals ran too late,
// either the poll times out (recorded, asserted on the test goroutine) or
// the process is simply killed by the OS default handler and the test
// binary itself would report the failure.
//
// Unregistration after this run is covered separately and more directly by
// TestSignalHandlerUnregisteredAfterStopFunctionReturns; this test does not
// duplicate that mechanism.
func TestSignalRegisteredBeforeFirstInit(t *testing.T) {
	var initOrder []string
	var sendErr error
	var observedStopInTime bool

	defFirst := def("signal-first", func() plugin.Plugin {
		return &hooked{
			onInit: func(ctx *plugin.Context) error {
				initOrder = append(initOrder, "first")
				sendErr = signalSelf(syscall.SIGTERM)
				select {
				case <-ctx.Done():
					observedStopInTime = errors.Is(ctx.Err(), context.Canceled)
				case <-time.After(testTimeout):
				}
				return nil
			},
			onStop: func(context.Context) error {
				initOrder = append(initOrder, "first-stop")
				return nil
			},
		}
	})
	defSecond := def("signal-second", func() plugin.Plugin {
		return &hooked{
			onInit: func(*plugin.Context) error {
				initOrder = append(initOrder, "second")
				return nil
			},
		}
	})

	app := newTestApp(t, defFirst, defSecond)
	ch := runAsyncProcess(app, quietConfig(t, "")...)
	res := awaitResult(t, ch)

	require.NoError(t, sendErr, "向测试进程发送 SIGTERM 失败")
	require.True(t, observedStopInTime,
		"发出 SIGTERM 后 App 从未观察到停止请求；signal.Notify 可能注册得太晚，或根本没注册")

	require.Error(t, res.err, "启动期间收到停止请求必须让 run 返回错误")
	assert.Equal(t, 1, res.code, "启动被中止属于运行失败，退出码应为 1")
	assert.Contains(t, res.err.Error(), "初始化", "错误必须点明中止发生在初始化阶段")
	assert.Contains(t, res.err.Error(), stopReasonSignal, "错误应说明启动期间收到的是停止请求")
	assert.Equal(t, []string{"first", "first-stop"}, initOrder,
		"second 的 Init 绝不能被调用；已经 Init 成功的 first 必须被反向清理（Stop 调用）")
}

// TestSignalWatcherContinuesAfterFirstSignal ensures the process adapter keeps
// draining its signal channel until explicit unregistration. requestStop is
// intentionally idempotent, but the watcher must not abandon the channel
// after the first signal and leave a second SIGINT/SIGTERM unread.
func TestSignalWatcherContinuesAfterFirstSignal(t *testing.T) {
	var calls atomic.Int32
	stop := watchProcessSignals(func() { calls.Add(1) })
	defer stop()

	require.NoError(t, signalSelf(syscall.SIGTERM), "发送第一次信号失败")
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 1 }, testTimeout),
		"监听器没有消费第一次信号")

	require.NoError(t, signalSelf(syscall.SIGINT), "发送第二次信号失败")
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 2 }, testTimeout),
		"监听器在第一次回调后退出，第二次信号无人读取")
	assert.Equal(t, int32(2), calls.Load())
}

// TestSignalDuringRunTriggersCleanShutdown pins rule #2: once the app has
// finished starting and is blocked in the run loop, SIGINT and SIGTERM must
// both produce a clean shutdown -- every Closer's Stop called, exit code 0
// -- exactly as if requestStop(stopReasonSignal) had been called directly.
//
// This is the only place in the suite that drives the real OS signal path
// end to end while the app is actually running (the Init-stage test above
// exercises the same delivery path, but during startup instead), so it is
// the one place a signal set registered incompletely -- say, SIGTERM but not
// SIGINT -- would be caught.
func TestSignalDuringRunTriggersCleanShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			var stopped bool

			app := newTestApp(t,
				def("signal-running", func() plugin.Plugin {
					return &hooked{
						onStop: func(context.Context) error { stopped = true; return nil },
					}
				}),
				liveness("signal-liveness"),
			)

			ch := runAsyncProcess(app, quietConfig(t, "")...)
			awaitReady(t, app)

			require.NoError(t, signalSelf(sig), "向测试进程发送信号失败")
			res := awaitResult(t, ch)

			require.NoError(t, res.err, "信号触发的关机应当是干净的，不应带错误返回")
			assert.Equal(t, 0, res.code, "signal 触发的停止属于正常退出，退出码应为 0")
			assert.Equal(t, stopReasonSignal, app.stopReason, "停止原因应记录为 signal 而非其它来源")
			assert.True(t, stopped, "运行中的插件必须收到 Stop")
		})
	}
}

// TestSignalStopDuringInitStageAbortsRemainingInitAndUnwinds pins the first
// half of rule #3: a stop requested while the Init stage is still walking
// instances must stop it from reaching the next instance's Init, and must
// unwind only what actually finished initializing.
//
// "signal-stage-a" sorts before "signal-stage-b" (catalog.Freeze sorts
// Definitions by Name, so declaration order does not decide this), so a's
// Init is guaranteed to run first; it calls requestStop directly rather than
// sending a real signal, per the task's guidance that only the signal tests
// themselves need to go through the OS.
func TestSignalStopDuringInitStageAbortsRemainingInitAndUnwinds(t *testing.T) {
	var app *App
	var bInitCalled, aStopped, bStopped bool

	defA := def("signal-stage-a", func() plugin.Plugin {
		return &hooked{
			onInit: func(*plugin.Context) error {
				app.requestStop(stopReasonSignal)
				return nil
			},
			onStop: func(context.Context) error { aStopped = true; return nil },
		}
	})
	defB := def("signal-stage-b", func() plugin.Plugin {
		return &hooked{
			onInit: func(*plugin.Context) error { bInitCalled = true; return nil },
			onStop: func(context.Context) error { bStopped = true; return nil },
		}
	})
	app = newTestApp(t, defA, defB)

	ch := runAsync(app, quietConfig(t, "")...)
	res := awaitResult(t, ch)

	require.Error(t, res.err, "启动被中止必须返回错误")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "初始化", "错误必须点明中止发生在初始化阶段")
	assert.False(t, bInitCalled, "停止请求之后，b 的 Init 绝不能被调用")
	assert.True(t, aStopped, "a 已经 Init 成功，必须在反向清理中收到 Stop")
	assert.False(t, bStopped, "b 从未 Init 成功，不应该有 Stop 被调用")
}

// TestSignalStopDuringMigrateStageAbortsRemainingMigrateAndUnwinds pins the
// second half of rule #3, for the Migrate stage.
//
// Unlike the Init-stage case, by the time Migrate runs both instances have
// already Init'd successfully -- migrateAll only runs after initAll
// completes in full. So this test's discriminating assertion is the
// opposite of the Init-stage test's: b's Stop *must* be called even though
// its Migrate never ran, because b already holds whatever resources its own
// Init acquired.
func TestSignalStopDuringMigrateStageAbortsRemainingMigrateAndUnwinds(t *testing.T) {
	var app *App
	var bMigrateCalled, aStopped, bStopped bool

	defA := def("signal-stage-a", func() plugin.Plugin {
		return &hooked{
			onMigrate: func(*plugin.Context) error {
				app.requestStop(stopReasonSignal)
				return nil
			},
			onStop: func(context.Context) error { aStopped = true; return nil },
		}
	})
	defB := def("signal-stage-b", func() plugin.Plugin {
		return &hooked{
			onMigrate: func(*plugin.Context) error { bMigrateCalled = true; return nil },
			onStop:    func(context.Context) error { bStopped = true; return nil },
		}
	})
	app = newTestApp(t, defA, defB)

	args := append(quietConfig(t, ""), "--migrate")
	ch := runAsync(app, args...)
	res := awaitResult(t, ch)

	require.Error(t, res.err, "启动被中止必须返回错误")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "迁移", "错误必须点明中止发生在迁移阶段")
	assert.False(t, bMigrateCalled, "停止请求之后，b 的 Migrate 绝不能被调用")
	assert.True(t, aStopped, "a 已经 Init 成功，必须在反向清理中收到 Stop")
	assert.True(t, bStopped, "b 即便没跑到 Migrate，此前也已经 Init 成功，同样必须被反向清理")
}

// TestSignalStopDuringStartStageAbortsRemainingStartAndUnwinds pins the
// third half of rule #3, for the Runner/Start stage. Both instances have
// already Init'd (and, since no --migrate is passed, migrateAll is skipped
// entirely) by the time startRunners begins.
func TestSignalStopDuringStartStageAbortsRemainingStartAndUnwinds(t *testing.T) {
	var app *App
	var bStartCalled, aStopped, bStopped bool

	defA := def("signal-stage-a", func() plugin.Plugin {
		return &hooked{
			onStart: func(*plugin.Context) error {
				app.requestStop(stopReasonSignal)
				return nil
			},
			onStop: func(context.Context) error { aStopped = true; return nil },
		}
	})
	defB := def("signal-stage-b", func() plugin.Plugin {
		return &hooked{
			onStart: func(*plugin.Context) error { bStartCalled = true; return nil },
			onStop:  func(context.Context) error { bStopped = true; return nil },
		}
	})
	app = newTestApp(t, defA, defB)

	ch := runAsync(app, quietConfig(t, "")...)
	res := awaitResult(t, ch)

	require.Error(t, res.err, "启动被中止必须返回错误")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "启动", "错误必须点明中止发生在启动阶段")
	assert.False(t, bStartCalled, "停止请求之后，b 的 Start 绝不能被调用")
	assert.True(t, aStopped, "a 已经 Init 成功，必须在反向清理中收到 Stop")
	assert.True(t, bStopped, "b 已经 Init 成功，即便没跑到 Start，也必须被反向清理")
}

// TestSignalStopDuringOpenTrafficStageAbortsRemainingOpenAndUnwinds pins the
// last half of rule #3, for the TrafficOpener/OpenTraffic stage -- the
// second half of the two-phase readiness protocol.
func TestSignalStopDuringOpenTrafficStageAbortsRemainingOpenAndUnwinds(t *testing.T) {
	var app *App
	var bOpenCalled, aStopped, bStopped bool

	defA := def("signal-stage-a", func() plugin.Plugin {
		return &hooked{
			onOpen: func(*plugin.Context) error {
				app.requestStop(stopReasonSignal)
				return nil
			},
			onStop: func(context.Context) error { aStopped = true; return nil },
		}
	})
	defB := def("signal-stage-b", func() plugin.Plugin {
		return &hooked{
			onOpen: func(*plugin.Context) error { bOpenCalled = true; return nil },
			onStop: func(context.Context) error { bStopped = true; return nil },
		}
	})
	app = newTestApp(t, defA, defB)

	ch := runAsync(app, quietConfig(t, "")...)
	res := awaitResult(t, ch)

	require.Error(t, res.err, "启动被中止必须返回错误")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "开放流量", "错误必须点明中止发生在开放流量阶段")
	assert.False(t, bOpenCalled, "停止请求之后，b 的 OpenTraffic 绝不能被调用")
	assert.True(t, aStopped, "a 已经 Init 成功，必须在反向清理中收到 Stop")
	assert.True(t, bStopped, "b 已经 Init 成功，即便没跑到 OpenTraffic，也必须被反向清理")
}

// TestSignalHandlerUnregisteredAfterStopFunctionReturns pins rule #4: the
// stop function watchProcessSignals returns must actually undo signal.Notify,
// rather than merely no-opping while leaving the OS-level registration (and
// its goroutine) alive. A leaked registration would keep firing on every
// later SIGINT/SIGTERM sent by any other test in this package, silently
// calling requestStop on an App nobody is looking at any more.
//
// The pin compares two Apps rather than sending a bare signal and hoping
// nothing reacts: appA registers and immediately unregisters; appB
// registers afterwards and is the only one still listening when the single
// SIGTERM below is sent. Go's os/signal package delivers one incoming
// signal to *every* channel currently registered for it -- see Notify's own
// doc comment -- so if appA's registration had leaked, that same SIGTERM
// would flip appA.stopRequested() to true as well. That is exactly the
// failure this test is built to catch, and it cannot produce a false pass:
// the only way appA.stopRequested() ends up false is if stopA() really
// stopped appA's goroutine from observing the signal.
//
// Sending the real signal only while appB (still registered) is listening
// also means the process's OS-default disposition never gets a chance to
// run, so this assertion cannot kill the test binary.
func TestSignalHandlerUnregisteredAfterStopFunctionReturns(t *testing.T) {
	var observedA, observedB atomic.Bool
	stopA := watchProcessSignals(func() { observedA.Store(true) })
	stopA()

	stopB := watchProcessSignals(func() { observedB.Store(true) })
	defer stopB()

	require.NoError(t, signalSelf(syscall.SIGTERM), "向测试进程发送信号失败")
	require.True(t, signalWaitUntil(observedB.Load, testTimeout),
		"appB 仍在监听，理应在信号发出后观察到停止请求；如果这里超时，说明测试环境本身在拦截信号，而非生产代码的问题")

	// appA and appB observe the very same OS signal at essentially the same
	// time (Notify's delivery loop reaches every registered channel before
	// returning), so the wait above already gave a leaked appA listener its
	// chance to react. This assertion, not any additional sleep, is what
	// determines the outcome.
	assert.False(t, observedA.Load(),
		"appA 已经调用 stop()，之后不应再收到信号；出现该情况说明 watchProcessSignals 返回的 stop 函数未能解除 signal.Notify 注册")
}
