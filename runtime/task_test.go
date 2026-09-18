package runtime

import (
	"context"
	"errors"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

func newTestTaskRuntime(onCritical func(string)) *taskRuntime {
	return newTaskRuntime(nil, onCritical)
}

func taskIdentity(key plugin.Key) plugin.Identity {
	return plugin.Identity{Plugin: key}.Normalized()
}

// workloadTestRuntime builds a task runtime whose budgets and attribution are
// handed to it directly, for the cases whose subject is the budget arithmetic
// rather than the configuration and the plan that normally produce it. The
// wiring of those two through a real composition is covered separately by
// TestConfiguredWorkloadBudgetIsChargedThroughTheRealPlan.
func workloadTestRuntime(logger log.Logger, limits map[plugin.WorkloadKey]int, owner map[plugin.Key]plugin.WorkloadKey) *taskRuntime {
	tasks := newTaskRuntime(logger, nil)
	tasks.configureWorkloadBudget(limits, func(identity plugin.Identity) (plugin.WorkloadKey, bool) {
		workload, attributed := owner[identity.Normalized().Plugin]
		return workload, attributed
	})
	return tasks
}

// blockingTask returns a task body that runs until release is closed, so a test
// can hold a workload's budget saturated for as long as it needs to.
func blockingTask(release <-chan struct{}) func(context.Context) {
	return func(context.Context) { <-release }
}

func TestWorkloadGoroutineBudgetRefusesTheSubmissionThatWouldExceedIt(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	tasks := workloadTestRuntime(logger,
		map[plugin.WorkloadKey]int{"sast": 2},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	defer close(release)
	for range 2 {
		require.True(t, tasks.submit(owner, blockingTask(release), false),
			"a workload with a budget of two admits its first two concurrent tasks")
	}

	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"the third concurrent task exceeds the workload's budget")

	reports := tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Equal(t, plugin.WorkloadKey("sast"), reports[0].Workload)
	assert.Equal(t, 2, reports[0].Limit)
	assert.Equal(t, 2, reports[0].Running, "a refusal charges no slot")
	assert.Equal(t, uint64(1), reports[0].Rejected)

	require.Len(t, logger.entries, 1, "exactly the refused submission is reported")
	entry := logger.entries[0]
	assert.Equal(t, "warn", entry.level)
	rendered := logger.text()
	assert.Contains(t, rendered, "workload goroutine budget")
	assert.Contains(t, rendered, "dispatcher")
	assert.Contains(t, rendered, "sast")
	assert.Contains(t, rendered, "limit 2")
	assert.Contains(t, rendered, "running 2")

	// GoCritical is metered by the same budget: a critical task occupies a slot
	// for the process's lifetime, so a workload that declares a bound is bounded
	// for its long-lived capabilities too.
	assert.False(t, tasks.submit(owner, func(context.Context) {}, true),
		"a critical submission is refused by the same budget")
	reports = tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Equal(t, 2, reports[0].Running, "a refused critical submission charges no slot either")
	assert.Equal(t, uint64(2), reports[0].Rejected)
	assert.Zero(t, tasks.criticalTaskCount(), "and leaves no critical capability behind")
}

// TestWorkloadBudgetRefusalIsNotReportedAsAClosedAdmissionWindow keeps the two
// reasons a submission is refused apart. Context.Go returns a bool, so the
// caller cannot tell them apart, and the log line is the only place an operator
// can: one condition is a Plugin submitting outside its Start hook, which is a
// code defect, and the other is a workload that has used up the concurrency its
// configuration allows, which is a capacity decision.
func TestWorkloadBudgetRefusalIsNotReportedAsAClosedAdmissionWindow(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	tasks := workloadTestRuntime(logger,
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"})
	owner := taskIdentity("dispatcher")

	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"submitting before Start is refused for the admission reason")

	require.True(t, tasks.openStart(owner))
	release := make(chan struct{})
	defer close(release)
	require.True(t, tasks.submit(owner, blockingTask(release), false))
	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"and the exhausted budget is refused for the budget reason")

	require.Len(t, logger.entries, 2)
	assert.Contains(t, logger.entries[0].msg, "rejected outside owning Plugin Start")
	assert.NotContains(t, logger.entries[0].msg, "budget")
	assert.Contains(t, logger.entries[1].msg, "workload goroutine budget")
	assert.NotContains(t, logger.entries[1].msg, "Start")
}

func TestWorkloadBudgetIsPerWorkloadAndLeavesUnownedPluginsUnbounded(t *testing.T) {
	t.Parallel()
	tasks := workloadTestRuntime(nil,
		map[plugin.WorkloadKey]int{"sast": 1, "coderanger": 2},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast", "scanner": "coderanger"})
	saturated := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(saturated))

	release := make(chan struct{})
	defer close(release)
	require.True(t, tasks.submit(saturated, blockingTask(release), false))
	assert.False(t, tasks.submit(saturated, func(context.Context) {}, false),
		"the first workload is at its limit")

	// Another workload is charged against its own budget, which the first
	// workload's exhaustion does not touch.
	scanner := taskIdentity("scanner")
	require.True(t, tasks.openStart(scanner))
	for index := range 2 {
		require.True(t, tasks.submit(scanner, blockingTask(release), false),
			"coderanger's own budget of two admits submission %d", index)
	}
	assert.False(t, tasks.submit(scanner, blockingTask(release), false),
		"and is enforced on its own terms, not by the other workload's limit")

	// A Plugin that belongs to no workload has no budget at all: there is
	// deliberately no process-wide ceiling, because one would ration the
	// unowned plugins a standby process exists to run.
	unowned := taskIdentity("unowned")
	require.True(t, tasks.openStart(unowned))
	for index := range 8 {
		require.True(t, tasks.submit(unowned, blockingTask(release), false),
			"an unowned Plugin is unbounded, so submission %d is admitted", index)
	}

	reports := tasks.workloadBudgets()
	require.Len(t, reports, 2, "an unowned Plugin contributes no budget to report")
	assert.Equal(t, plugin.WorkloadKey("coderanger"), reports[0].Workload, "reports are sorted by key")
	assert.Equal(t, 2, reports[0].Running)
	assert.Equal(t, uint64(1), reports[0].Rejected)
	assert.Equal(t, plugin.WorkloadKey("sast"), reports[1].Workload)
	assert.Equal(t, 1, reports[1].Running)
	assert.Equal(t, uint64(1), reports[1].Rejected)
}

// TestWorkloadBudgetBoundsConcurrencyRatherThanTotalSubmissions is the property
// that separates a budget from a submission cap. A slot has to come back when
// its task returns, or a long-running process would eventually be unable to
// submit anything at all.
func TestWorkloadBudgetBoundsConcurrencyRatherThanTotalSubmissions(t *testing.T) {
	t.Parallel()
	tasks := workloadTestRuntime(nil,
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	require.True(t, tasks.submit(owner, blockingTask(release), false))
	assert.False(t, tasks.submit(owner, func(context.Context) {}, false), "one task is the whole budget")

	// Releasing the running task and joining its scope proves the slot is back:
	// the run loop releases under the budget's own lock and only then reaches
	// the waiter it was counted against, so a returned stopPlugin observes a
	// budget that has already given the slot up.
	close(release)
	require.NoError(t, tasks.stopPlugin(owner, context.Background()))
	reports := tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Zero(t, reports[0].Running, "a returned task gives its slot back")
	assert.Equal(t, uint64(1), reports[0].Rejected, "the earlier refusal stays counted")

	// Many sequential submissions, each released before the next, never
	// exhaust a budget of one.
	for range 8 {
		next := make(chan struct{})
		require.True(t, tasks.submit(owner, blockingTask(next), false),
			"a budget of one admits one task at a time, however many times over")
		close(next)
		require.NoError(t, tasks.stopPlugin(owner, context.Background()))
	}
	assert.Zero(t, tasks.workloadBudgets()[0].Running)
}

func TestWorkloadBudgetAdmitsExactlyItsLimitUnderConcurrentSubmission(t *testing.T) {
	t.Parallel()
	// A concurrent test logs its refusals from many goroutines at once, so the
	// logger stays nil here: the capture helper used above is deliberately not
	// race-safe, and the subject of this test is the arithmetic.
	tasks := workloadTestRuntime(nil,
		map[plugin.WorkloadKey]int{"sast": 3},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	var accepted atomic.Int32
	var waiters sync.WaitGroup
	for range 32 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			if tasks.submit(owner, blockingTask(release), false) {
				accepted.Add(1)
			}
		}()
	}
	waiters.Wait()

	assert.Equal(t, int32(3), accepted.Load(),
		"charge is atomic, so a concurrent burst admits exactly the limit and no more")
	reports := tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Equal(t, 3, reports[0].Running)
	assert.Equal(t, uint64(29), reports[0].Rejected)

	close(release)
	require.NoError(t, tasks.stopPlugin(owner, context.Background()))
	require.NoError(t, tasks.drainRemaining(context.Background()))
}

// TestUnconfiguredBudgetBehavesExactlyAsBefore is the property every existing
// application depends on: a composition that declares no workload budget has
// nothing rationed, and the path through submit is untouched.
func TestUnconfiguredBudgetBehavesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	cases := []struct {
		name string
		// bounded is whether any workload in this composition declares a bound,
		// which decides whether the report has entries at all.
		bounded bool
		tasks   *taskRuntime
	}{
		{
			name:    "no budget configured at all",
			bounded: false,
			tasks:   newTaskRuntime(logger, nil),
		},
		{
			name:    "attribution but no limits",
			bounded: false,
			tasks: workloadTestRuntime(logger,
				map[plugin.WorkloadKey]int{},
				map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"}),
		},
		{
			// The submitting workload declares no bound even though another one
			// in the same composition does, so the budget is configured and the
			// submitting workload is still not rationed by it.
			name:    "a limit on a workload this Plugin does not belong to",
			bounded: true,
			tasks: workloadTestRuntime(logger,
				map[plugin.WorkloadKey]int{"coderanger": 1},
				map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"}),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tasks := testCase.tasks
			owner := taskIdentity("dispatcher")
			require.True(t, tasks.openStart(owner))
			release := make(chan struct{})
			defer close(release)
			for index := range 64 {
				require.True(t, tasks.submit(owner, blockingTask(release), false),
					"submission %d must be admitted; nothing bounded this workload", index)
			}

			reports := tasks.workloadBudgets()
			if !testCase.bounded {
				assert.Nil(t, reports,
					"a process whose workloads declare no bound reports no budget at all")
			}
			for _, report := range reports {
				assert.NotEqual(t, plugin.WorkloadKey("sast"), report.Workload,
					"a workload with no bound is absent from the report rather than shown at zero")
			}
			assert.Empty(t, logger.entries, "nothing is refused, so nothing is logged")
		})
	}
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
	assert.Zero(t, tasks.criticalTaskCount(), "a non-critical task is no long-lived capability")

	require.True(t, tasks.openStart(owner))
	for range 2 {
		require.True(t, tasks.submit(owner, func(ctx context.Context) { <-ctx.Done() }, true))
	}
	tasks.closeStart(owner)
	assert.Equal(t, 2, tasks.criticalTaskCount(), "every admitted critical task is counted")
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
	assert.Zero(t, tasks.criticalTaskCount())
}

func TestConcurrentSubmitCloseAndShutdownStayConsistent(t *testing.T) {
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

func TestShutdownOfOnePluginLeavesAnotherPluginsTasksRunning(t *testing.T) {
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

func TestShutdownReportsATaskThatIgnoresCancellationRatherThanWaitingForever(t *testing.T) {
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

func TestShutdownDrainReapsEveryScopeNoLifecycleStageClaimed(t *testing.T) {
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
	require.NoError(t, tasks.drainRemaining(context.Background()), "draining twice is a no-op")
}

func TestShutdownDrainAggregatesFailuresAcrossPlugins(t *testing.T) {
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

// workloadRuntimeTestConfig writes a configuration whose only non-preamble
// content is the workloads block, and returns the arguments a run is started
// with.
func workloadRuntimeTestConfig(t *testing.T, workloads string) []string {
	t.Helper()
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: 2s\n" + workloads
	return []string{"--config", writeRuntimeTestConfig(t, contents)}
}

// TestConfiguredWorkloadBudgetIsChargedThroughTheRealPlan is the wiring test
// for this budget. The unit tests above supply limits and attribution directly,
// which cannot tell whether either half ever reaches a real process; this one
// starts from the configuration and the composition root and asserts that the
// limit read at bootstrap and the attribution resolved from the built plan meet
// in the same task runtime.
func TestConfiguredWorkloadBudgetIsChargedThroughTheRealPlan(t *testing.T) {
	// accepted is written inside the Start hook, which runs on the goroutine
	// driving the run, and read here only after the ready boundary has been
	// crossed -- the close and the receive that crossing consists of are what
	// orders the two, exactly as they do for every other lifecycle observation
	// this package makes.
	var accepted []bool
	release := make(chan struct{})

	definition := plugin.Define("dispatcher", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			for range 3 {
				accepted = append(accepted, ctx.Go(func(context.Context) { <-release }))
			}
			return nil
		},
		// Nothing here prepares traffic; the hook exists so the runtime's
		// liveness requirement is satisfied by something other than the tasks
		// under test, which are non-critical by design.
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	bundle := plugin.WorkloadOf("sast", plugin.BundleOf(definition), plugin.WithReplicas(1))

	app := newApp([]plugin.Bundle{bundle})
	app.ready = make(chan struct{})
	result := executeRuntimeTest(app, workloadRuntimeTestConfig(t, "workloads:\n  sast:\n    max_goroutines: 2\n")...)
	awaitRuntimeTestReady(t, app)

	assert.Equal(t, []bool{true, true, false}, accepted,
		"the configured budget bounds the workload's managed tasks end to end")

	reports := app.tasks.workloadBudgets()
	require.Len(t, reports, 1, "only the one declared workload has a budget")
	assert.Equal(t, plugin.WorkloadKey("sast"), reports[0].Workload)
	assert.Equal(t, 2, reports[0].Limit)
	assert.Equal(t, 2, reports[0].Running)
	assert.Equal(t, uint64(1), reports[0].Rejected)

	close(release)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
}

// TestWorkloadBudgetWithoutAPlanBoundsNothing covers the one path that reaches
// the budget before a plan exists. A Plugin cannot submit that early, but the
// attribution has to answer rather than panic if it is ever asked, and "belongs
// to no workload" is the answer that keeps such a submission unbounded instead
// of silently refusing it.
func TestWorkloadBudgetWithoutAPlanBoundsNothing(t *testing.T) {
	t.Parallel()
	app := newApp(nil)
	tasks := newTaskRuntime(nil, nil)
	limits, err := workloadTaskLimits(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, limits)
	tasks.configureWorkloadBudget(limits, app.workloadOf)

	workload, attributed := app.workloadOf(taskIdentity("anything"))
	assert.False(t, attributed, "a nil plan attributes nothing rather than panicking")
	assert.Empty(t, workload)

	owner := taskIdentity("anything")
	require.True(t, tasks.openStart(owner))
	release := make(chan struct{})
	defer close(release)
	require.True(t, tasks.submit(owner, blockingTask(release), false))
}

// TestAManagedTaskCarriesItsWorkloadAsAProfilerLabel pins the attribution a CPU
// profile is read by.
//
// The label is the only thing that can answer "which workload is burning this
// CPU". Stack frames answer which Plugin, but workload membership is decided at
// composition and appears in no frame, so two processes running the same binary
// attribute the same function to different workloads. A profile taken on a busy
// host is exactly where that question is asked, and it cannot be reconstructed
// afterwards.
func TestAManagedTaskCarriesItsWorkloadAsAProfilerLabel(t *testing.T) {
	t.Parallel()

	const scanner plugin.Key = "sast-worker"
	tasks := workloadTestRuntime(nil, nil, map[plugin.Key]plugin.WorkloadKey{scanner: "sast"})
	owner := taskIdentity(scanner)
	require.True(t, tasks.openStart(owner))

	labelled := make(chan string, 1)
	require.True(t, tasks.submit(owner, func(ctx context.Context) {
		value, _ := pprof.Label(ctx, "workload")
		labelled <- value
	}, false))

	select {
	case got := <-labelled:
		assert.Equal(t, "sast", got, "a managed task runs under its workload's profiler label")
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the managed task did not run")
	}

	// No budget was configured, which is the ordinary case: attribution must not
	// depend on a workload having declared max_goroutines, because the two
	// answer unrelated questions.
	assert.Nil(t, tasks.workloadBudgets(), "this process bounds nothing, and is still attributed")
}

// TestAnUnownedPluginsTaskIsLeftUnlabelled is the other half, and it is a
// correctness rule rather than a preference.
//
// Profiler labels are inherited by every goroutine started under them, and the
// plugins that belong to no workload are the shared ones -- the Web server's
// accept loop above all -- whose children serve every workload's routes.
// Labelling that loop as unowned would file each of those requests under a name
// that denies the workload actually being served, which is worse than leaving
// the shared work untagged.
func TestAnUnownedPluginsTaskIsLeftUnlabelled(t *testing.T) {
	t.Parallel()

	const shared plugin.Key = "web"
	tasks := workloadTestRuntime(nil, map[plugin.WorkloadKey]int{"sast": 4}, nil)
	owner := taskIdentity(shared)
	require.True(t, tasks.openStart(owner))

	labelled := make(chan bool, 1)
	require.True(t, tasks.submit(owner, func(ctx context.Context) {
		_, present := pprof.Label(ctx, "workload")
		labelled <- present
	}, false))

	select {
	case present := <-labelled:
		assert.False(t, present,
			"a Plugin belonging to no workload carries no workload label, so its children are not misattributed")
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the managed task did not run")
	}
}

// TestALabelledTaskStillChargesAndReleasesItsBudgetSlot keeps the two features
// that read the same attribution from drifting apart. The label is derived from
// attribution and the budget slot from attribution plus a configured limit, and
// a submission has to get both right at once: a task that ran labelled but
// uncharged would silently unbound the workload it names.
func TestALabelledTaskStillChargesAndReleasesItsBudgetSlot(t *testing.T) {
	t.Parallel()

	const scanner plugin.Key = "sast-worker"
	tasks := workloadTestRuntime(nil,
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{scanner: "sast"})
	owner := taskIdentity(scanner)
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	running := make(chan string, 1)
	require.True(t, tasks.submit(owner, func(ctx context.Context) {
		value, _ := pprof.Label(ctx, "workload")
		running <- value
		<-release
	}, false))

	select {
	case got := <-running:
		require.Equal(t, "sast", got)
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the managed task did not run")
	}
	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"the labelled task took the workload's only slot")

	close(release)
	require.Eventually(t, func() bool {
		reports := tasks.workloadBudgets()
		return len(reports) == 1 && reports[0].Running == 0
	}, runtimeTestTimeout, time.Millisecond, "the slot is given back when the labelled task returns")
}
