package runtime

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// errFakeExit is the sentinel the osExit replacement panics with, so a test
// can tell "the process exited" apart from a genuine panic escaping the
// framework.
var errFakeExit = new(struct{ _ byte })

// withFakeExit replaces the package's osExit hook for the duration of a test
// and returns a func reporting the code the process adapter passed to it.
//
// The replacement panics rather than returning, because the real os.Exit
// never returns: letting the fake return would run code after the "exit"
// that production could never reach.
func withFakeExit(t *testing.T) func() (int, bool) {
	t.Helper()
	var (
		mu     sync.Mutex
		code   int
		called bool
	)
	previous := osExit
	osExit = func(value int) {
		mu.Lock()
		code, called = value, true
		mu.Unlock()
		panic(errFakeExit)
	}
	t.Cleanup(func() { osExit = previous })

	return func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		return code, called
	}
}

// runFakeExit invokes fn with the faked os.Exit panic confined to this call.
func runFakeExit(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil && recovered != errFakeExit {
			panic(recovered)
		}
	}()
	fn()
}

// withConfinedForceQuit wraps the real forceQuit for the duration of a test so
// that the faked exit panic cannot escape the signal watcher's goroutine: a
// panic there would abort the whole test binary rather than fail one test. The
// returned channel reports that the escalation ran, after the real body --
// notice, logger flush, osExit -- has been executed.
func withConfinedForceQuit(t *testing.T) <-chan struct{} {
	t.Helper()
	forced := make(chan struct{}, 1)
	previous := forceQuit
	forceQuit = func() {
		runFakeExit(previous)
		select {
		case forced <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { forceQuit = previous })
	return forced
}

// withCapturedStderr redirects the process adapter's stderr stream for the
// duration of a test and returns an accessor for the text written to it. The
// buffer is guarded because the forced-termination notice is written from the
// signal watcher's goroutine.
func withCapturedStderr(t *testing.T) func() string {
	t.Helper()
	captured := &fakeStderr{}
	previous := stderr
	stderr = captured
	t.Cleanup(func() { stderr = previous })
	return captured.String
}

type fakeStderr struct {
	mu       sync.Mutex
	contents []byte
}

func (f *fakeStderr) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contents = append(f.contents, p...)
	return len(p), nil
}

func (f *fakeStderr) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.contents)
}

// signalSelf sends sig to the test binary's own process. It never touches
// *testing.T because it is called from goroutines other than the one running
// the test.
func signalSelf(sig syscall.Signal) error {
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return process.Signal(sig)
}

// signalWaitUntil polls condition until it holds or the timeout elapses. The
// sleep is only an upper bound on a failing case; success is always observed
// by condition returning true.
func signalWaitUntil(condition func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(200 * time.Microsecond)
	}
	return false
}

func TestExitCodesReachTheProcessThroughOsExit(t *testing.T) {
	failing := withFakeExit(t)
	runFakeExit(func() { exitApp(newRuntimeTestApp(), runtimeTestConfig(t, time.Second)) })
	code, called := failing()
	require.True(t, called, "the process adapter terminates through osExit instead of returning")
	assert.Equal(t, 1, code,
		"a startup failure that exits 0 looks healthy to every orchestrator")

	succeeding := withFakeExit(t)
	runFakeExit(func() {
		exitApp(newRuntimeTestApp(), append([]string{"doctor"}, runtimeTestConfig(t, time.Second)...))
	})
	code, called = succeeding()
	require.True(t, called)
	assert.Equal(t, 0, code, "a successful run must not be reported as a failure")
}

func TestPreAppFailuresExitOneBeforeLoggingIsConfigured(t *testing.T) {
	exitCode := withFakeExit(t)
	runFakeExit(func() { exitWithError(assert.AnError) })
	code, called := exitCode()
	require.True(t, called)
	assert.Equal(t, 1, code)
}

func TestRunProcessTakesItsArgumentsFromTheProcess(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = append([]string{"xbc-test", "doctor"}, runtimeTestConfig(t, time.Second)...)

	exitCode := withFakeExit(t)
	runFakeExit(func() { runProcess(newRuntimeTestApp()) })
	code, called := exitCode()
	require.True(t, called)
	assert.Equal(t, 0, code, "runProcess reads os.Args[1:], so the doctor subcommand is honoured")
}

// TestRunExitsThroughTheProcessAdapter covers the one-line entry point
// applications actually call: it must terminate the process rather than return
// to main, and it must carry the command's exit code out with it.
//
// The other half of Run -- that New() with no Bundles composes the frozen
// process catalog -- is not observable from here, because Run returns no App.
// It is asserted by TestNewWithoutBundlesFreezesTheProcessCatalog, which
// inspects the resulting plan against a sentinel declared into that catalog.
func TestRunExitsThroughTheProcessAdapter(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = append([]string{"xbc-test", "doctor"}, runtimeTestConfig(t, time.Second)...)

	exitCode := withFakeExit(t)
	runFakeExit(Run)
	code, called := exitCode()
	require.True(t, called, "Run terminates the process instead of returning to main")
	assert.Equal(t, 0, code)
}

func TestSignalDuringRunTriggersACleanShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			var stopped atomic.Bool
			definition := plugin.Define("signal-running", func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
				OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
				Stop: func(*runtimeTestValue, context.Context) error {
					stopped.Store(true)
					return nil
				},
			}})

			app := newRuntimeTestApp(definition)
			result := make(chan runtimeTestResult, 1)
			go func() {
				code, err := executeWithSignals(app, runtimeTestConfig(t, time.Second))
				result <- runtimeTestResult{code: code, err: err}
			}()
			awaitRuntimeTestReady(t, app)

			require.NoError(t, signalSelf(sig))
			completed := awaitRuntimeTestResult(t, result)
			require.NoError(t, completed.err, "an operator-requested stop is not a failure")
			assert.Equal(t, 0, completed.code)
			assert.Equal(t, stopReasonSignal, app.currentStopReason(),
				"both supported signals are registered, not just one of them")
			assert.True(t, stopped.Load(), "running plugins receive Stop")
		})
	}
}

// TestSignalWatcherEscalatesRepeatedRequestsInsteadOfDroppingThem is the
// routing half of the repeat-signal contract: the first signal must reach the
// graceful path and every later one the escalation path. It deliberately
// passes plain counters rather than the real forceQuit, so a routing
// regression shows up as a counter, not as a dead test binary.
func TestSignalWatcherEscalatesRepeatedRequestsInsteadOfDroppingThem(t *testing.T) {
	var stopRequests, forced atomic.Int32
	stop := watchProcessSignals(func() { stopRequests.Add(1) }, func() { forced.Add(1) })
	defer stop()

	require.NoError(t, signalSelf(syscall.SIGTERM))
	require.True(t, signalWaitUntil(func() bool { return stopRequests.Load() >= 1 }, runtimeTestTimeout),
		"the first signal was never consumed")
	assert.Zero(t, forced.Load(), "the first signal asks for a graceful shutdown, it must never force termination")

	require.NoError(t, signalSelf(syscall.SIGINT))
	require.True(t, signalWaitUntil(func() bool { return forced.Load() >= 1 }, runtimeTestTimeout),
		"the watcher abandoned its channel after the first callback, losing the operator's second request")

	require.NoError(t, signalSelf(syscall.SIGINT))
	require.True(t, signalWaitUntil(func() bool { return forced.Load() >= 2 }, runtimeTestTimeout),
		"every repeat after the first must escalate, not just the second one")
	assert.Equal(t, int32(1), stopRequests.Load(),
		"a repeat must not re-enter the graceful path, which is exactly the path that is already stuck")
}

// TestForcedTerminationExplainsItselfBeforeExitingOne pins the process tail of
// the escalation path. It calls forceQuit directly on the test's own
// goroutine, so the faked exit panic stays inside runFakeExit.
func TestForcedTerminationExplainsItselfBeforeExitingOne(t *testing.T) {
	notice := withCapturedStderr(t)
	exitCode := withFakeExit(t)

	runFakeExit(forceQuit)

	code, called := exitCode()
	require.True(t, called, "forced termination must actually terminate the process")
	assert.Equal(t, 1, code, "an abandoned shutdown did not complete, so it is a failure, not a clean exit")
	assert.Equal(t, "xbc: repeated stop signal, abandoning graceful shutdown and terminating now\n", notice(),
		"a process that vanishes without a word on the second Ctrl-C is its own mystery")
}

// TestRepeatedSignalEndsAnApplicationStuckInAPluginHook is the case the
// escalation exists for: a plugin hook that never returns keeps App.execute
// from ever handing control back to the runtime, so no amount of stop
// requesting can unwind the application. The first signal must still only
// request a stop; the second must end the process.
//
// forceQuit is replaced with a wrapper that confines the faked exit panic --
// escalation runs on the signal watcher's goroutine, where an escaping panic
// would take the whole test binary down instead of failing this test. The
// stuck hook is released and the execute goroutine joined before the test
// returns, so no watcher survives into the next test with a live signal
// registration.
func TestRepeatedSignalEndsAnApplicationStuckInAPluginHook(t *testing.T) {
	notice := withCapturedStderr(t)
	exitCode := withFakeExit(t)
	forced := withConfinedForceQuit(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	definition := plugin.Define("stuck-start", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error {
			close(entered)
			<-release
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	args := runtimeTestConfig(t, time.Second)
	result := make(chan runtimeTestResult, 1)
	go func() {
		code, err := executeWithSignals(app, args)
		result <- runtimeTestResult{code: code, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the plugin hook that is supposed to block was never entered")
	}

	require.NoError(t, signalSelf(syscall.SIGINT))
	require.True(t, signalWaitUntil(app.stopRequested, runtimeTestTimeout),
		"the first signal did not even request a stop")
	assert.Equal(t, stopReasonSignal, app.currentStopReason())
	_, called := exitCode()
	require.False(t, called,
		"the first signal must give the graceful shutdown a chance, not terminate the process")

	require.NoError(t, signalSelf(syscall.SIGINT))
	select {
	case <-forced:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the repeated signal never terminated a process no stop request could unwind")
	}
	code, called := exitCode()
	require.True(t, called)
	assert.Equal(t, 1, code)
	assert.Contains(t, notice(), "xbc: repeated stop signal")

	close(release)
	completed := awaitRuntimeTestResult(t, result)
	assert.Equal(t, 1, completed.code, "the stop request that arrived during startup still aborts the run")
	require.Error(t, completed.err)
}

// TestShutdownUnregistersTheSignalHandlerWhenStopReturns compares two watchers rather
// than sending a bare signal and hoping nothing reacts. os/signal delivers one
// incoming signal to every channel currently registered for it, so a leaked
// registration in the already-stopped watcher would observe the same SIGTERM
// that the live one does.
func TestShutdownUnregistersTheSignalHandlerWhenStopReturns(t *testing.T) {
	var observedStopped, observedLive atomic.Bool
	stopStopped := watchProcessSignals(func() { observedStopped.Store(true) }, func() {})
	stopStopped()
	stopStopped()

	stopLive := watchProcessSignals(func() { observedLive.Store(true) }, func() {})
	defer stopLive()

	require.NoError(t, signalSelf(syscall.SIGTERM))
	require.True(t, signalWaitUntil(observedLive.Load, runtimeTestTimeout),
		"the still-registered watcher never saw the signal; the environment, not the code, is intercepting it")
	assert.False(t, observedStopped.Load(),
		"stop() must undo signal.Notify rather than leave the OS-level registration alive")
}
