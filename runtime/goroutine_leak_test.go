package runtime

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/xbcio/xbc/plugin"
)

// verifyNoGoroutineLeaks is this package's goleak.VerifyNone with the one
// exception the framework cannot eliminate, excluded by exact function name
// rather than by loosening the check.
//
// The exception is lumberjack's mill goroutine. TestBootedJSONSinkCarries
// TheConfiguredIdentity (service_identity_test.go) deliberately boots with a
// real log file sink, and lumberjack starts one millRun goroutine per Logger
// on the first write through it (openExistingOrNew calls mill; lumberjack
// v2.2.1 lumberjack.go:387-393). That goroutine ends only when millCh is
// closed, and nothing in v2.2.1 ever closes it - Logger.Close just closes
// the file (lumberjack.go:165) - so one millRun outlives every test for the
// rest of the process. That is the dependency's design, not a cleanup this
// package forgot: log.Close() does close the file sink, and the goroutine
// survives it regardless. With -count>1 it is therefore present at every
// later check, which is why only the repeated runs went red. Ignoring this
// one name masks nothing else - a leaked watcher, task, or file handle still
// fails the test that finds it.
//
// The name below is spelled with %2e for the module-version dot because that
// is the spelling the toolchain produces: the linker encodes it that way in
// the symbol itself (go tool nm shows the same string) and goleak compares
// the argument literally against the name parsed out of runtime.Stack output,
// without unescaping it (v1.3.0 internal/stack parseFuncName).
func verifyNoGoroutineLeaks(t *testing.T) {
	t.Helper()
	goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun"))
}

// TestExecuteSignalUnwindLeavesNoGoroutineBehind guards the full
// Execute -> signal -> unwind path end to end: a running process, started
// through the same executeWithSignals adapter runProcess uses, carrying a
// managed task, a critical task, and a traffic-opening plugin, stopped by a
// real SIGTERM and walked back through Unwind and the drain phase.
//
// The leak check is the discriminating assertion. Every other test in this
// package already pins that Stop runs, that managed tasks are canceled, and
// that the drain phase reaps task scopes the reverse walk never reached; none
// of them notice a goroutine that outlives the process by blocking forever
// instead of returning. A regression that left the signal watcher, a managed
// task, or a drain worker running after Execute has returned would pass every
// one of those assertions and only show up here.
func TestExecuteSignalUnwindLeavesNoGoroutineBehind(t *testing.T) {
	defer verifyNoGoroutineLeaks(t)

	taskStarted := make(chan struct{})
	criticalStarted := make(chan struct{})
	definition := plugin.Define("leak-guard", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			require.True(t, ctx.Go(func(taskContext context.Context) {
				close(taskStarted)
				<-taskContext.Done()
			}))
			require.True(t, ctx.GoCritical(func(taskContext context.Context) {
				close(criticalStarted)
				<-taskContext.Done()
			}))
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	app := newRuntimeTestApp(definition)

	result := make(chan runtimeTestResult, 1)
	go func() {
		code, err := executeWithSignals(app, runtimeTestConfig(t, time.Second))
		result <- runtimeTestResult{code: code, err: err}
	}()
	awaitRuntimeTestReady(t, app)

	select {
	case <-taskStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("managed task did not start")
	}
	select {
	case <-criticalStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("critical task did not start")
	}

	require.NoError(t, signalSelf(syscall.SIGTERM))

	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
}
