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

// TestExecuteSignalUnwindLeavesNoGoroutineBehind guards the full
// Execute -> signal -> unwind path end to end: a running process, started
// through the same executeWithSignals adapter runProcess uses, carrying a
// managed task, a critical task, and a traffic-opening plugin, stopped by a
// real SIGTERM and walked back through Unwind and the drain phase.
//
// goleak.VerifyNone is the discriminating assertion. Every other test in this
// package already pins that Stop runs, that managed tasks are canceled, and
// that the drain phase reaps task scopes the reverse walk never reached; none
// of them notice a goroutine that outlives the process by blocking forever
// instead of returning. A regression that left the signal watcher, a managed
// task, or a drain worker running after Execute has returned would pass every
// one of those assertions and only show up here.
func TestExecuteSignalUnwindLeavesNoGoroutineBehind(t *testing.T) {
	defer goleak.VerifyNone(t)

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
