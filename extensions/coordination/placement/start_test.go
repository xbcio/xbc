package placement

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// ── a runtime that refuses managed tasks ───────────────────────────────────

// refusingHost answers every managed-task submission with false, which is what
// the runtime does for a submission made outside the Start hook's admission
// window. testHost always accepts, so start's refusal branch -- and the
// wait-group bookkeeping it owes -- is reachable only through a host like this
// one. It is minimal on purpose: nothing is ever admitted, so the host has no
// task to run and no gate to hold.
type refusingHost struct {
	executionCtx context.Context
	gate         chan struct{}
	submissions  atomic.Int32
}

var _ plugin.RuntimeHost = (*refusingHost)(nil)

func newRefusingHost() *refusingHost {
	gate := make(chan struct{})
	close(gate)
	return &refusingHost{executionCtx: context.Background(), gate: gate}
}

func (h *refusingHost) ExecutionContext() context.Context { return h.executionCtx }
func (h *refusingHost) Logger() log.Logger                { return log.Nop() }
func (h *refusingHost) TrafficGate() <-chan struct{}      { return h.gate }

func (h *refusingHost) SubmitTask(_ plugin.Identity, _ func(context.Context), _ bool) bool {
	h.submissions.Add(1)
	return false
}

func (h *refusingHost) RequestShutdown(_ plugin.Identity, _ string) bool { return false }

func (h *refusingHost) context() *plugin.Context {
	return plugin.NewRuntimeContext(h, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

// awaitStop runs stop and fails the test if it has not returned inside the
// budget. stop waits on the managed-task group, so a submission that was
// refused but still counted leaves a count no goroutine will ever decrement,
// and every later shutdown blocks on it until its budget expires. The defect is
// a hang, so the assertion has to be a test failure rather than a hung suite.
func awaitStop(t *testing.T, value *Placement) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- value.stop(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return: a refused managed-task submission left a count in the placement's wait group")
	}
}

// TestARefusedRenewalTaskDoesNotWedgeStop pins the wait-group bookkeeping on the
// renewal branch of start's refusal path.
//
// start counts the renewal loop before submitting it, and stop waits on that
// group. A runtime that refuses the submission runs no goroutine, so the count
// has to be given back on the way out; otherwise the process can never shut
// down, which is far worse than the loop not existing.
func TestARefusedRenewalTaskDoesNotWedgeStop(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err, "the process holds a slot, so start takes the renewal branch")

	host := newRefusingHost()
	err = value.start(host.context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accepting the lease renewal task")
	assert.Equal(t, int32(1), host.submissions.Load(), "the renewal loop is submitted exactly once")

	awaitStop(t, value)
}

// TestARefusedStandbyTaskDoesNotWedgeStop is the same defect on the other
// branch. The standby loop has its own Add/Done pair, so a fix that only
// covered the renewal path would leave a standby process unable to stop -- and
// a standby is the shape that is meant to be replaceable at any moment.
func TestARefusedStandbyTaskDoesNotWedgeStop(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker, WithStandbyRetry(10*time.Millisecond))
	decision, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted, "every slot is held elsewhere, so start takes the standby branch")

	host := newRefusingHost()
	err = value.start(host.context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accepting the standby retry task")
	assert.Equal(t, int32(1), host.submissions.Load(), "the standby loop is submitted exactly once")

	awaitStop(t, value)
}

// ── the guards at the top of start ─────────────────────────────────────────

// TestStartRefusesANilContextASecondStartAndAStartAfterStop pins the three
// guards that decide whether a Start hook may admit managed work at all.
//
// Each one protects a different mistake: a nil context is a caller that
// bypassed the runtime, a second Start is a lifecycle that ran twice and would
// otherwise leave two loops renewing the same slots, and a Start after Stop is
// a resume attempt that would renew slots the process has already given back.
func TestStartRefusesANilContextASecondStartAndAStartAfterStop(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	err = value.start(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a non-nil plugin context")

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))

	err = value.start(host.context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "called more than once")

	// The run is alive here, so the guard that answers is the lifecycle state
	// rather than the cancelled context.
	require.NoError(t, value.stop(context.Background()))

	err = value.start(host.context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "called after Stop")

	host.requestStop()
	host.awaitTasks(t)
}

// TestStartRefusesARunThatIsAlreadyStopping covers the guard between the nil
// check and the lifecycle switch: the execution context the Context reports is
// the run's, and a run that has already been asked to stop must not admit a
// loop that would then have no quiet moment to observe.
func TestStartRefusesARunThatIsAlreadyStopping(t *testing.T) {
	value := mustNew(t, newMemoryLocker())
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	host := newTestHost()
	host.requestStop()

	err = value.start(host.context())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, value.started, "a refused Start leaves the instance unstarted")
}

// ── a replacement placement.New ────────────────────────────────────────────

// TestASupersededPlacementNamesTheReplacement pins the diagnostic a second
// placement.New in one process produces.
//
// installedPlacement is process-global, and the package-wide Definition reads
// it when the Definition is built. So a process that built a Definition against
// one Placement and then saw another New call land holds a Definition bound to
// a Placement that is no longer the process's -- and that never resolved,
// because only the process's own placement is the one the runtime consults.
// Starting it has to name the replacement: "requires a decided placement"
// describes a composition the operator did not write and sends them looking in
// the wrong place.
//
// The test builds the state it needs itself and puts the global back, so it
// depends on no ordering relative to the other tests in this package. The
// ordering it does depend on is inside the test: the Definition's binding is
// taken before the second New, because that is what makes the binding stale.
func TestASupersededPlacementNamesTheReplacement(t *testing.T) {
	previous := installedPlacement.Load()
	t.Cleanup(func() { installedPlacement.Store(previous) })

	locker := newMemoryLocker()

	// The Placement the graph's Definition was built against.
	superseded := mustNew(t, locker)
	bound, err := installed()
	require.NoError(t, err)
	require.Same(t, superseded, bound, "the Definition binds whatever New installed when it was built")

	// A second New replaces it process-wide, decides the hosted set, and holds
	// a slot: from the outside this composition looks correct.
	replacement := mustNew(t, locker)
	decision, err := replacement.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Equal(t, []plugin.WorkloadKey{"sast"}, decision.Hosted)
	assert.False(t, superseded.resolved, "the superseded placement is not the one the runtime decided with")

	host := newTestHost()
	err = superseded.start(host.context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another placement.New call replaced this process's placement")
	assert.NotContains(t, err.Error(), "requires a decided placement",
		"the generic message names a different mistake, which is what made this defect hard to diagnose")
}
