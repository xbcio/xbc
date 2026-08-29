package runtime

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runAsyncProcess exercises the private process adapter, including real
// SIGINT/SIGTERM registration, while still returning instead of calling
// os.Exit. Tests not specifically about signals use runAsync and therefore
// exercise the side-effect-free Execute boundary.
func runAsyncProcess(app *App, args ...string) <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		code, err := executeWithSignals(app, args)
		ch <- runResult{code: code, err: err}
	}()
	return ch
}

// --- exit-code capture ----------------------------------------------------

// withFakeExit replaces the package's osExit hook for the duration of a test
// and returns a func reporting the code runProcess passed to it.
//
// The replacement panics with a sentinel rather than returning, because the
// real os.Exit never returns: letting the fake return would run code after
// the "exit" that production would never reach, and a test asserting on that
// code would be asserting on a path that cannot happen.
func withFakeExit(t *testing.T) (code func() (int, bool)) {
	t.Helper()

	var (
		mu     sync.Mutex
		got    int
		called bool
	)
	prev := osExit
	osExit = func(c int) {
		mu.Lock()
		got, called = c, true
		mu.Unlock()
		panic(errFakeExit)
	}
	t.Cleanup(func() { osExit = prev })

	return func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		return got, called
	}
}

// errFakeExit is the sentinel withFakeExit's replacement panics with, so a
// test can tell "the app exited" apart from a genuine panic escaping the
// framework.
var errFakeExit = new(struct{ _ byte })

// --- D. the process boundary ----------------------------------------------

// TestProcessRunnerRoutesExecuteExitCodeThroughOsExit pins the one thing the
// runProcess adapter does that Execute does not: hand its exit code to os.Exit.
//
// Everything else about exit codes is tested against App.Execute directly (see
// execute_command_test.go), because Execute returns rather than terminating. But the
// seam between "run finished with code N" and "the process exits with N" is
// only crossed here, and it is exactly where a refactor can silently drop a
// non-zero code -- an application that fails to start but exits 0 looks
// healthy to systemd, Kubernetes and every CI runner.
//
// The empty catalog is used as the failure trigger simply because it fails
// fast and deterministically without needing a plugin, a signal, or a
// timeout; the subject under test is the code's route to osExit, not the
// reason the code is 1.
func TestProcessRunnerRoutesExecuteExitCodeThroughOsExit(t *testing.T) {
	exitCode := withFakeExit(t)

	app := newTestApp(t)

	// osExit is faked to panic, because the real one never returns; recover
	// it here so the "process exit" stays confined to this test.
	func() {
		defer func() {
			if r := recover(); r != nil && r != errFakeExit {
				panic(r)
			}
		}()
		exitApp(app, nil)
	}()

	code, called := exitCode()
	require.True(t, called, "Run 必须通过 osExit 终止进程，而不是直接返回")
	assert.Equal(t, 1, code,
		"空 catalog 的启动失败必须以非零码退出——退出 0 会让编排系统把失败当成健康")
}
