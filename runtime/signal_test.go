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

	require.NoError(t, sendErr, "Failed to send SIGTERM to the test process")
	require.True(t, observedStopInTime,
		"After sending SIGTERM, the App never observed the shutdown request; signal.Notify may have registered too late, or not at all")

	require.Error(t, res.err, "Receiving a shutdown request during startup must make run return an error")
	assert.Equal(t, 1, res.code, "An aborted startup is considered a runtime failure, exit code should be 1")
	assert.Contains(t, res.err.Error(), "initialization", "The error must specify that the abortion occurred during initialization")
	assert.Contains(t, res.err.Error(), stopReasonSignal, "The error should explain that a shutdown request was received during startup")
	assert.Equal(t, []string{"first", "first-stop"}, initOrder,
		"second's Init must never be called; the already successfully initialized first must be reverse cleaned up (Stop called)")
}

// TestSignalWatcherContinuesAfterFirstSignal ensures the process adapter keeps
// draining its signal channel until explicit unregistration. requestStop is
// intentionally idempotent, but the watcher must not abandon the channel
// after the first signal and leave a second SIGINT/SIGTERM unread.
func TestSignalWatcherContinuesAfterFirstSignal(t *testing.T) {
	var calls atomic.Int32
	stop := watchProcessSignals(func() { calls.Add(1) })
	defer stop()

	require.NoError(t, signalSelf(syscall.SIGTERM), "Failed to send the first signal")
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 1 }, testTimeout),
		"The listener did not consume the first signal")

	require.NoError(t, signalSelf(syscall.SIGINT), "Failed to send the second signal")
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 2 }, testTimeout),
		"The listener exits after the first callback, the second signal is unread")
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

			require.NoError(t, signalSelf(sig), "Failed to send a signal to the test process")
			res := awaitResult(t, ch)

			require.NoError(t, res.err, "The shutdown triggered by a signal should be clean, without error return")
			assert.Equal(t, 0, res.code, "The shutdown triggered by a signal is considered a normal exit, exit code should be 0")
			assert.Equal(t, stopReasonSignal, app.stopReason, "The reason for shutdown should be recorded as signal, not from other sources")
			assert.True(t, stopped, "Running plugins must receive Stop")
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

	require.Error(t, res.err, "An aborted startup must return an error")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "initialization", "The error must specify that the abortion occurred during initialization")
	assert.False(t, bInitCalled, "After a shutdown request, b's Init must never be called")
	assert.True(t, aStopped, "a has already successfully Init, it must receive Stop during reverse cleanup")
	assert.False(t, bStopped, "b has never successfully Init, it should not have Stop called")
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

	require.Error(t, res.err, "An aborted startup must return an error")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "migration", "The error must specify that the abortion occurred during migration")
	assert.False(t, bMigrateCalled, "After a shutdown request, b's Migrate must never be called")
	assert.True(t, aStopped, "a has already been Init successfully, must receive Stop in reverse cleanup")
	assert.True(t, bStopped, "b has already been Init successfully even if it didn't reach Migrate, must still be cleaned up in reverse")
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

	require.Error(t, res.err, "Startup interruption must return an error")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "startup", "Error must indicate that the interruption occurred during the startup phase")
	assert.False(t, bStartCalled, "After a stop request, b's Start must never be called")
	assert.True(t, aStopped, "a has already been Init successfully, must receive Stop in reverse cleanup")
	assert.True(t, bStopped, "b has already been Init successfully, must still be cleaned up in reverse even if it didn't reach Start")
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

	require.Error(t, res.err, "Startup interruption must return an error")
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.err.Error(), "open traffic", "Error must indicate that the interruption occurred during the open traffic phase")
	assert.False(t, bOpenCalled, "After a stop request, b's OpenTraffic must never be called")
	assert.True(t, aStopped, "a has already been Init successfully, must receive Stop in reverse cleanup")
	assert.True(t, bStopped, "b has already been Init successfully, must still be cleaned up in reverse even if it didn't reach OpenTraffic")
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

	require.NoError(t, signalSelf(syscall.SIGTERM), "Failed to send signal to test process")
	require.True(t, signalWaitUntil(observedB.Load, testTimeout),
		"appB is still listening, should observe a stop request after the signal is sent; if this times out, it indicates the test environment is intercepting signals, not the production code")

	// appA and appB observe the very same OS signal at essentially the same
	// time (Notify's delivery loop reaches every registered channel before
	// returning), so the wait above already gave a leaked appA listener its
	// chance to react. This assertion, not any additional sleep, is what
	// determines the outcome.
	assert.False(t, observedA.Load(),
		"appA has already called stop(), should not receive any further signals; if this occurs, it indicates the stop function returned by watchProcessSignals failed to deregister signal.Notify")
}
