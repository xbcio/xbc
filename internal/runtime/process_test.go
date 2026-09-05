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

	"github.com/xbcio/xbc/internal/autoload"
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

// TestRunComposesTheProcessCatalogAndExitsThroughIt covers the one-line entry
// point applications actually call. It freezes the process-wide autoload
// catalog, which is why it asserts on the exit route rather than on a
// composition of its own.
func TestRunComposesTheProcessCatalogAndExitsThroughIt(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = append([]string{"xbc-test", "doctor"}, runtimeTestConfig(t, time.Second)...)

	exitCode := withFakeExit(t)
	runFakeExit(Run)
	code, called := exitCode()
	require.True(t, called, "Run terminates the process instead of returning to main")
	assert.Equal(t, 0, code)
	assert.NotNil(t, autoload.Freeze(), "Run composes the frozen process catalog")
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

func TestSignalWatcherKeepsDrainingAfterTheFirstSignal(t *testing.T) {
	var calls atomic.Int32
	stop := watchProcessSignals(func() { calls.Add(1) })
	defer stop()

	require.NoError(t, signalSelf(syscall.SIGTERM))
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 1 }, runtimeTestTimeout),
		"the first signal was never consumed")

	require.NoError(t, signalSelf(syscall.SIGINT))
	require.True(t, signalWaitUntil(func() bool { return calls.Load() >= 2 }, runtimeTestTimeout),
		"the watcher abandoned its channel after the first callback, losing the operator's second request")
}

// TestSignalHandlerIsUnregisteredAfterStopReturns compares two watchers rather
// than sending a bare signal and hoping nothing reacts. os/signal delivers one
// incoming signal to every channel currently registered for it, so a leaked
// registration in the already-stopped watcher would observe the same SIGTERM
// that the live one does.
func TestSignalHandlerIsUnregisteredAfterStopReturns(t *testing.T) {
	var observedStopped, observedLive atomic.Bool
	stopStopped := watchProcessSignals(func() { observedStopped.Store(true) })
	stopStopped()
	stopStopped()

	stopLive := watchProcessSignals(func() { observedLive.Store(true) })
	defer stopLive()

	require.NoError(t, signalSelf(syscall.SIGTERM))
	require.True(t, signalWaitUntil(observedLive.Load, runtimeTestTimeout),
		"the still-registered watcher never saw the signal; the environment, not the code, is intercepting it")
	assert.False(t, observedStopped.Load(),
		"stop() must undo signal.Notify rather than leave the OS-level registration alive")
}
