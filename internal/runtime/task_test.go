package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func newTestTaskRuntime(onCritical func(string)) *taskRuntime {
	return newTaskRuntime(nil, onCritical)
}

func taskIdentity(key plugin.Key) plugin.Identity {
	return plugin.Identity{Plugin: key}.Normalized()
}

func TestTasksAreAdmittedOnlyBetweenOpenStartAndCloseStart(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(nil)
	owner := taskIdentity("owner")

	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"a task submitted before openStart has no owning Start to belong to")

	require.True(t, tasks.openStart(owner))
	assert.False(t, tasks.submit(owner, nil, false), "a nil task is rejected instead of spawned")
	assert.False(t, tasks.submit(taskIdentity("other"), func(context.Context) {}, false),
		"admission is per plugin, never process-wide")

	done := make(chan struct{})
	require.True(t, tasks.submit(owner, func(context.Context) { close(done) }, false))
	<-done
	tasks.closeStart(owner)

	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"admission closes when the owning Start returns")
	assert.Equal(t, 1, tasks.spawnedCount(), "spawnedCount counts every admitted task, not the running ones")
	require.NoError(t, tasks.drainRemaining(context.Background()))
}

func TestClosedAdmissionRejectsEveryFurtherSubmission(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(nil)
	owner := taskIdentity("owner")
	require.True(t, tasks.openStart(owner))
	tasks.closeAdmission()

	assert.False(t, tasks.openStart(owner), "a shutting-down runtime never reopens admission")
	assert.False(t, tasks.submit(owner, func(context.Context) {}, false))
	assert.Zero(t, tasks.spawnedCount())
}

func TestConcurrentSubmitCloseAndStopStayConsistent(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(func(string) {})
	owner := taskIdentity("racy")
	require.True(t, tasks.openStart(owner))

	var waiters sync.WaitGroup
	for range 16 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			tasks.submit(owner, func(ctx context.Context) { <-ctx.Done() }, false)
		}()
	}
	waiters.Add(2)
	go func() { defer waiters.Done(); tasks.closeAdmission() }()
	go func() { defer waiters.Done(); _ = tasks.stopPlugin(owner, context.Background()) }()
	waiters.Wait()

	require.NoError(t, tasks.stopPlugin(owner, context.Background()))
	require.NoError(t, tasks.drainRemaining(context.Background()))
}

func TestStoppingOnePluginLeavesAnotherPluginsTasksRunning(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(nil)
	first, second := taskIdentity("first"), taskIdentity("second")
	secondStillRunning := make(chan struct{})
	secondCanceled := make(chan struct{})

	require.True(t, tasks.openStart(first))
	require.True(t, tasks.submit(first, func(ctx context.Context) { <-ctx.Done() }, false))
	tasks.closeStart(first)
	require.True(t, tasks.openStart(second))
	require.True(t, tasks.submit(second, func(ctx context.Context) {
		close(secondStillRunning)
		<-ctx.Done()
		close(secondCanceled)
	}, false))
	tasks.closeStart(second)
	<-secondStillRunning

	require.NoError(t, tasks.stopPlugin(first, context.Background()))
	select {
	case <-secondCanceled:
		t.Fatal("stopping one plugin canceled another plugin's task scope")
	default:
	}

	require.NoError(t, tasks.drainRemaining(context.Background()))
	<-secondCanceled
	assert.NoError(t, tasks.stopPlugin(first, context.Background()), "stopping an already-drained plugin is a no-op")
}

func TestACriticalTaskReturnIsAFailureOnlyOutsideShutdown(t *testing.T) {
	t.Parallel()
	for name, shuttingDown := range map[string]bool{"while running": false, "during shutdown": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			escalated := make(chan string, 1)
			tasks := newTestTaskRuntime(func(reason string) { escalated <- reason })
			owner := taskIdentity("critical")
			require.True(t, tasks.openStart(owner))

			returned := make(chan struct{})
			release := make(chan struct{})
			require.True(t, tasks.submit(owner, func(context.Context) {
				close(returned)
				<-release
			}, true))
			<-returned
			tasks.closeStart(owner)
			// The flag has to be visible before the task returns: it is read
			// by runTask's deferred judgment, not by stopPlugin. For the same
			// reason the release below must precede stopPlugin, whose cancel
			// would otherwise make the return look scope-driven.
			if shuttingDown {
				tasks.shuttingDown.Store(true)
				close(release)
				assert.NoError(t, tasks.stopPlugin(owner, context.Background()),
					"a critical task returning during its own shutdown is expected")
				assert.Empty(t, escalated)
				return
			}

			close(release)
			select {
			case reason := <-escalated:
				assert.Contains(t, reason, "xbc: plugin critical critical managed task unexpectedly returned")
			case <-time.After(runtimeTestTimeout):
				t.Fatal("an unprompted critical return did not escalate")
			}
			err := tasks.stopPlugin(owner, context.Background())
			require.Error(t, err, "the escalated failure is also reported against its owning plugin")
			assert.Contains(t, err.Error(), "critical managed task unexpectedly returned")
		})
	}
}

func TestACriticalTaskThatReturnsAfterItsScopeIsCanceledIsNotAFailure(t *testing.T) {
	t.Parallel()
	var escalations atomic.Int32
	tasks := newTestTaskRuntime(func(string) { escalations.Add(1) })
	owner := taskIdentity("well-behaved")
	require.True(t, tasks.openStart(owner))
	require.True(t, tasks.submit(owner, func(ctx context.Context) { <-ctx.Done() }, true))
	tasks.closeStart(owner)

	require.NoError(t, tasks.stopPlugin(owner, context.Background()),
		"a critical task that returns because its scope was canceled did its job")
	assert.Zero(t, escalations.Load())
}

func TestPanicsAreRecordedAndOnlyCriticalOnesEscalate(t *testing.T) {
	t.Parallel()
	for name, critical := range map[string]bool{"critical": true, "plain": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var escalations atomic.Int32
			tasks := newTestTaskRuntime(func(string) { escalations.Add(1) })
			owner := taskIdentity("panicking")
			require.True(t, tasks.openStart(owner))

			panicked := make(chan struct{})
			require.True(t, tasks.submit(owner, func(context.Context) {
				defer close(panicked)
				panic(errors.New("task exploded"))
			}, critical))
			<-panicked
			tasks.closeStart(owner)

			err := tasks.stopPlugin(owner, context.Background())
			require.Error(t, err, "a panic is recorded against its owning plugin regardless of criticality")
			assert.Contains(t, err.Error(), "xbc: plugin panicking managed task panic: task exploded")
			if critical {
				assert.Equal(t, int32(1), escalations.Load())
			} else {
				assert.Zero(t, escalations.Load(), "a non-critical panic is logged, not escalated")
			}
		})
	}
}

func TestATaskThatIgnoresCancellationIsReportedRatherThanWaitedForForever(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(nil)
	owner := taskIdentity("stubborn")
	release := make(chan struct{})
	defer close(release)
	require.True(t, tasks.openStart(owner))
	require.True(t, tasks.submit(owner, func(context.Context) { <-release }, false))
	tasks.closeStart(owner)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	err := tasks.stopPlugin(owner, expired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin stubborn managed tasks did not exit within the shutdown budget")
}

func TestDrainRemainingReapsEveryScopeNoLifecycleStageClaimed(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(nil)
	canceled := make(chan struct{}, 2)
	for _, key := range []plugin.Key{"abandoned-a", "abandoned-b"} {
		owner := taskIdentity(key)
		require.True(t, tasks.openStart(owner))
		require.True(t, tasks.submit(owner, func(ctx context.Context) {
			<-ctx.Done()
			canceled <- struct{}{}
		}, false))
		tasks.closeStart(owner)
	}

	require.NoError(t, tasks.drainRemaining(context.Background()))
	assert.Len(t, canceled, 2, "every leftover scope is canceled and joined")
	assert.Equal(t, 2, tasks.spawnedCount())
	require.NoError(t, tasks.drainRemaining(context.Background()), "draining twice is a no-op")
}

func TestDrainRemainingAggregatesFailuresAcrossPlugins(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskRuntime(func(string) {})
	release := make(chan struct{})
	defer close(release)
	for _, key := range []plugin.Key{"slow-a", "slow-b"} {
		owner := taskIdentity(key)
		require.True(t, tasks.openStart(owner))
		require.True(t, tasks.submit(owner, func(context.Context) { <-release }, false))
		tasks.closeStart(owner)
	}

	expired, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-expired.Done()
	err := tasks.drainRemaining(expired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slow-a")
	assert.Contains(t, err.Error(), "slow-b")
}
