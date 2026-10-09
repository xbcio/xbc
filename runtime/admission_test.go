package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// admissionTestRuntime is workloadTestRuntime with the hosted answer under the
// test's control, because "may this quota be charged at all" is the question
// the admission path asks that the managed-task path never does.
func admissionTestRuntime(limits map[plugin.WorkloadKey]int, owner map[plugin.Key]plugin.WorkloadKey, hosted map[plugin.WorkloadKey]bool) *taskRuntime {
	tasks := newTaskRuntime(nil, nil)
	tasks.configureWorkloadBudget(limits, func(identity plugin.Identity) (plugin.WorkloadKey, bool) {
		workload, attributed := owner[identity.Normalized().Plugin]
		return workload, attributed
	}, func(key plugin.WorkloadKey) bool { return hosted[key] })
	return tasks
}

func acquireOrFail(t *testing.T, admission plugin.Admission) func() {
	t.Helper()
	release, err := admission.Acquire(context.Background())
	require.NoError(t, err)
	require.NotNil(t, release)
	return release
}

// TestAdmissionChargesTheSameQuotaManagedTasksCharge is the whole point of the
// limiter: a unit of work a Plugin runs on goroutines the framework did not
// start counts against the workload's budget exactly as a managed task does,
// because one workload has one number.
func TestAdmissionChargesTheSameQuotaManagedTasksCharge(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	admission := tasks.admissionFor(owner, "")
	require.NotNil(t, admission)
	release := acquireOrFail(t, admission)

	reports := tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Equal(t, 1, reports[0].Running, "an admitted unit of unmanaged work occupies a slot")

	assert.False(t, tasks.submit(owner, func(context.Context) {}, false),
		"and a managed task submitted afterwards finds the workload at its limit")

	release()
	assert.Zero(t, tasks.workloadBudgets()[0].Running, "releasing gives the slot back")

	assert.True(t, tasks.submit(owner, func(context.Context) {}, false),
		"which is what lets the managed-task path proceed again")
	require.NoError(t, tasks.drainRemaining(context.Background()))
}

// TestAdmissionWaitsForARunningUnitToGiveItsSlotBack pins the blocking half.
// A refusal is not available to a queue worker -- it would have to fail a task
// it already dequeued -- so the limiter waits instead.
func TestAdmissionWaitsForARunningUnitToGiveItsSlotBack(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	require.True(t, tasks.submit(owner, blockingTask(release), false))

	admission := tasks.admissionFor(owner, "")
	acquired := make(chan func(), 1)
	go func() {
		slot, err := admission.Acquire(context.Background())
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- slot
	}()

	select {
	case <-acquired:
		t.Fatal("admission was granted while the workload was at its limit")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case slot := <-acquired:
		require.NotNil(t, slot, "the waiter proceeds once the running task gives its slot back")
		slot()
	case <-time.After(5 * time.Second):
		t.Fatal("admission never woke after a slot came free")
	}
}

// TestAdmissionReportsTheContextsErrorWhenTheWaitEndsFirst keeps a cancelled
// wait from taking a slot it never waited for: the caller returns the error and
// the quota is untouched.
func TestAdmissionReportsTheContextsErrorWhenTheWaitEndsFirst(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	release := make(chan struct{})
	defer close(release)
	require.True(t, tasks.submit(owner, blockingTask(release), false))
	admission := tasks.admissionFor(owner, "")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	slot, err := admission.Acquire(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the wait reports the reason it ended")
	assert.Nil(t, slot, "and hands back no release for a slot it never took")

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	slot, err = admission.Acquire(cancelled)
	assert.ErrorIs(t, err, context.Canceled, "an already-finished context is answered before the first check")
	assert.Nil(t, slot)

	assert.Equal(t, 1, tasks.workloadBudgets()[0].Running, "neither ended wait charged anything")
}

// TestAdmissionWakesExactlyTheWaitersASlotCanServe exercises the release
// broadcast: every waiter wakes on every release and re-checks, so what decides
// who proceeds is the count under the lock and never the order of wake-ups.
func TestAdmissionWakesExactlyTheWaitersASlotCanServe(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	holder := make(chan struct{})
	require.True(t, tasks.submit(owner, blockingTask(holder), false))
	admission := tasks.admissionFor(owner, "")

	slots := make(chan func(), 2)
	var waiters sync.WaitGroup
	for range 2 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			slot, err := admission.Acquire(context.Background())
			if err == nil {
				slots <- slot
			}
		}()
	}

	close(holder)
	select {
	case slot := <-slots:
		require.Equal(t, 1, tasks.workloadBudgets()[0].Running,
			"the freed slot serves one waiter, not both")
		select {
		case <-slots:
			t.Fatal("a second waiter took a slot the budget did not have")
		case <-time.After(50 * time.Millisecond):
		}
		slot()
	case <-time.After(5 * time.Second):
		t.Fatal("no waiter woke after the holder released its slot")
	}

	select {
	case slot := <-slots:
		require.NotNil(t, slot, "the waiters give up one at a time")
		slot()
	case <-time.After(5 * time.Second):
		t.Fatal("the second waiter never took the slot the first gave back")
	}
	waiters.Wait()
	require.NoError(t, tasks.drainRemaining(context.Background()))
}

func TestAdmissionReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")
	require.True(t, tasks.openStart(owner))

	admission := tasks.admissionFor(owner, "")
	release := acquireOrFail(t, admission)
	release()
	release()
	assert.Zero(t, tasks.workloadBudgets()[0].Running,
		"a second release must not give back a slot the workload no longer holds")

	// The slot released once is still available: an over-eager release that kept
	// decrementing would take the workload below its real usage.
	second := acquireOrFail(t, admission)
	second()
	assert.Zero(t, tasks.workloadBudgets()[0].Running)
}

// TestAdmissionIsUnboundedWhereThereIsNoQuotaToCharge covers every path that
// answers "nothing to bound". None of them is an error: each is an ordinary
// composition, and a shared Plugin asking about a workload this process does
// not carry is the routine case of a process that hosts fewer roles than the
// application declares.
func TestAdmissionIsUnboundedWhereThereIsNoQuotaToCharge(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1, "coderanger": 2},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})

	cases := []struct {
		name     string
		identity plugin.Identity
		workload plugin.WorkloadKey
	}{
		{"a Plugin that belongs to no workload", taskIdentity("unowned"), ""},
		{"a workload that declares no budget", taskIdentity("dispatcher"), "coderanger"},
		{"a workload this process does not host", taskIdentity("dispatcher"), "unknown"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assert.Nil(t, tasks.admissionFor(testCase.identity, testCase.workload))
		})
	}

	unconfigured := newTaskRuntime(nil, nil)
	assert.Nil(t, unconfigured.admissionFor(taskIdentity("dispatcher"), "sast"),
		"a runtime whose workloads declare no budget has nothing to charge")
}

// TestConfiguredAdmissionIsResolvedThroughTheRealPlan is the wiring test for
// the limiter. The unit tests above supply limits, attribution and the hosted
// set directly, which cannot tell whether any of them ever reaches a Plugin:
// this one starts from the configuration and the composition root and asserts
// that the quota a Context hands out is the one the plan and the configuration
// describe, charged in the same currency as a managed task.
func TestConfiguredAdmissionIsResolvedThroughTheRealPlan(t *testing.T) {
	// Every observation is written inside the Start hook, which runs on the
	// goroutine driving the run, and read only after the ready boundary -- the
	// same ordering the budget wiring test relies on.
	var (
		admitted    bool
		ownErr      error
		namedErr    error
		unhostedErr error
		emptyKeyErr error
	)
	release := make(chan struct{})
	definition := plugin.Define("worker", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			// Spend the workload's whole budget through the managed-task path,
			// so anything the limiter grants afterwards would be over it.
			admitted = ctx.Go(func(context.Context) { <-release })

			ownErr = waitForAdmission(ctx.Admission())
			namedErr = waitForAdmission(ctx.AdmissionFor("sast"))
			unhostedErr = waitForAdmission(ctx.AdmissionFor("nobody"))
			emptyKeyErr = waitForAdmission(ctx.AdmissionFor(""))
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
	result := executeRuntimeTest(app, workloadRuntimeTestConfig(t, "workloads:\n  sast:\n    max_goroutines: 1\n")...)
	awaitRuntimeTestReady(t, app)

	require.True(t, admitted, "the composition admits a managed task into the workload")
	assert.ErrorIs(t, ownErr, context.DeadlineExceeded,
		"the Context's limiter draws on the quota the running managed task holds")
	assert.ErrorIs(t, namedErr, context.DeadlineExceeded,
		"and naming the same workload reaches the same exhausted quota")
	assert.NoError(t, unhostedErr, "a workload this process does not carry is unbounded rather than blocked")
	assert.NoError(t, emptyKeyErr, "the empty key names nothing and never blocks")

	reports := app.tasks.workloadBudgets()
	require.Len(t, reports, 1)
	assert.Equal(t, 1, reports[0].Running, "the running managed task is what the limiter found in the way")
	assert.Zero(t, reports[0].Rejected, "a limiter wait is not a refused submission")

	close(release)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
}

// waitForAdmission reports what a limiter that has no slot to grant says: it
// must block, so a wait that ends by itself means the quota was not exhausted.
func waitForAdmission(admission plugin.Admission) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	release, err := admission.Acquire(ctx)
	if release != nil {
		release()
	}
	return err
}

// TestAdmissionForAWorkloadABelongsToIsTheSameLimiter keeps the two entry
// points one behaviour: naming the workload a Plugin already belongs to must
// not produce a second, independent quota.
func TestAdmissionForAWorkloadABelongsToIsTheSameLimiter(t *testing.T) {
	t.Parallel()
	tasks := admissionTestRuntime(
		map[plugin.WorkloadKey]int{"sast": 1},
		map[plugin.Key]plugin.WorkloadKey{"dispatcher": "sast"},
		map[plugin.WorkloadKey]bool{"sast": true})
	owner := taskIdentity("dispatcher")

	first := acquireOrFail(t, tasks.admissionFor(owner, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := tasks.admissionFor(owner, "sast").Acquire(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"the named form of a workload the Plugin already belongs to draws on the same exhausted quota")
	first()

	second := acquireOrFail(t, tasks.admissionFor(owner, "sast"))
	second()
	assert.Zero(t, tasks.workloadBudgets()[0].Running)
}
