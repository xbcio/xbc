package placement

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// ── a store shaped for the stop window ─────────────────────────────────────

// stopWindowLocker is a lease store shaped for the moment between "the run was
// asked to stop" and "the shutdown hooks have run".
//
// Two of its behaviours make that moment observable, and both are behaviours
// real backends have. Acquisition ignores the caller's cancellation and can be
// held inside the store by the test: that is miniredis, and any client that
// consults its context only before the round trip, granting a slot to a caller
// whose run has already ended. Renewal and release, by contrast, report the
// caller's cancellation, which is what a client that does consult it reports --
// so a call made on an already-cancelled context is visibly a call that achieved
// nothing.
//
// Every call is counted before the context is consulted, so a test can tell "no
// call was made" apart from "the call was made and failed". That distinction is
// the whole point: the defects here are about calls that should not happen and
// calls that must not inherit a cancelled context.
type stopWindowLocker struct {
	inner *memoryLocker

	mu       sync.Mutex
	gate     chan struct{}
	entered  chan struct{}
	renews   int
	releases int
}

var _ lease.Locker = (*stopWindowLocker)(nil)

func newStopWindowLocker() *stopWindowLocker {
	return &stopWindowLocker{inner: newMemoryLocker(), entered: make(chan struct{}, 1)}
}

func (l *stopWindowLocker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (lease.Lease, bool, error) {
	l.mu.Lock()
	gate := l.gate
	l.mu.Unlock()
	if gate != nil {
		select {
		case l.entered <- struct{}{}:
		default:
		}
		// Deliberately not selecting on ctx.Done(): this is the store that
		// grants a slot to a caller whose run has already been cancelled.
		<-gate
	}
	won, acquired, err := l.inner.TryAcquire(ctx, key, ttl)
	if err != nil || !acquired {
		return nil, false, err
	}
	return &stopWindowLease{inner: won, store: l}, true, nil
}

// holdAcquisitions makes every later acquisition block inside the store until
// releaseAcquisitions lets it go. Acquisitions made before it -- Resolve's own,
// which decides the placement the test then drives -- run untouched.
func (l *stopWindowLocker) holdAcquisitions() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gate = make(chan struct{})
}

func (l *stopWindowLocker) releaseAcquisitions() {
	l.mu.Lock()
	gate := l.gate
	l.mu.Unlock()
	close(gate)
}

// awaitAcquisition blocks until an acquisition is inside the store, so a test
// knows the cancellation it is about to deliver lands in the window it means to
// and not before it.
func (l *stopWindowLocker) awaitAcquisition(t *testing.T) {
	t.Helper()
	select {
	case <-l.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no acquisition reached the store")
	}
}

func (l *stopWindowLocker) renewAttempts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renews
}

func (l *stopWindowLocker) releaseAttempts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

type stopWindowLease struct {
	inner lease.Lease
	store *stopWindowLocker
}

func (l *stopWindowLease) Key() string   { return l.inner.Key() }
func (l *stopWindowLease) Owner() string { return l.inner.Owner() }

func (l *stopWindowLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	l.store.mu.Lock()
	l.store.renews++
	l.store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return l.inner.Renew(ctx, ttl)
}

func (l *stopWindowLease) Release(ctx context.Context) (bool, error) {
	l.store.mu.Lock()
	l.store.releases++
	l.store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return l.inner.Release(ctx)
}

// ── a stop is not a renewal failure ────────────────────────────────────────

// TestACleanStopIsNotRecordedAsARenewalFailure pins what the renewal loop must
// not do on its way out.
//
// The loop's select has three ready-able cases, and select picks uniformly among
// the ones that are ready, so a tick that becomes ready in the same moment the
// run is asked to stop is chosen about half the time. A round taken from there
// runs against a context that is already cancelled: every call fails,
// xbc_workload_lease_renew_failures_total counts failures no store ever caused,
// and readiness reports the process as degraded while it is leaving. An
// operator cannot alert on a counter that fires on every clean shutdown, so the
// round has to not happen at all.
//
// The tick is driven directly rather than waited for. The defect is a race
// resolved by the runtime's own coin flip, so a test that started the loop and
// hoped for the wrong side of it would pin nothing; the loop body is where the
// decision lives.
func TestACleanStopIsNotRecordedAsARenewalFailure(t *testing.T) {
	// The run's context is cancelled first, which is the order the runtime uses:
	// the execution context goes before any hook of this plugin runs.
	cancelled := newStopWindowLocker()
	value := mustNew(t, cancelled, WithRenewInterval(time.Hour))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()

	assert.False(t, value.renewTick(runCtx), "a tick that raced the stop signal ends the loop")
	assert.Zero(t, cancelled.renewAttempts(), "no renewal may be attempted once the run has been cancelled")
	assert.Zero(t, value.renewFailures.Load(), "a stop is not a renewal failure")
	require.NoError(t, value.renewalHealth(), "readiness must not go down because the process is stopping")

	// The other signal, reached through a Stop whose run was never cancelled --
	// a torn-down application, or a Stop called on its own. Neither signal
	// implies the other, so both have to be read.
	quiesced := newStopWindowLocker()
	other := mustNew(t, quiesced, WithRenewInterval(time.Hour))
	_, err = other.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	other.quiesce()

	assert.False(t, other.renewTick(context.Background()), "a tick that raced quiesce ends the loop")
	assert.Zero(t, quiesced.renewAttempts(), "a quiesced placement must not touch the store again")
	assert.Zero(t, other.renewFailures.Load())
}

// TestACancelledStoreCallIsStillARenewalFailure is the other side of the same
// fix, and the reason it is placed where it is.
//
// The cheap way to keep a shutdown out of the failure counter would be to stop
// counting context.Canceled where the renewal error is handled. That would also
// swallow the failure that matters most: a renewal whose own call was cancelled
// or timed out because the store had stopped answering, which is exactly the
// store outage the counter exists to report. The run here is alive, so a
// cancelled call can only have come from the store side, and it must count.
func TestACancelledStoreCallIsStillARenewalFailure(t *testing.T) {
	locker := newMemoryLocker()
	locker.renewErr = context.Canceled
	value := mustNew(t, locker, WithRenewInterval(time.Hour))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	assert.True(t, value.renewTick(context.Background()), "a failed renewal keeps the loop running")
	assert.Equal(t, uint64(1), value.renewFailures.Load(), "a cancelled store call is a renewal that did not happen")
	require.Error(t, value.renewalHealth(), "an unconfirmed claim takes readiness down while the process keeps hosting")
}

// ── a standby that wins on the way out ─────────────────────────────────────

// TestAStandbyThatWinsInsideTheStopWindowStillHandsTheSlotBack covers the race
// the design's release argument did not reach.
//
// The standby loop checks that the run is still alive before it asks for a slot,
// but the check cannot close the window: the run can be cancelled while the
// acquisition is in flight, and a store that consults the caller's context only
// before its round trip grants the slot anyway. The slot is then this process's
// to give back although it never hosted it -- and it is not in the held set, so
// nothing else on the shutdown path knows about it. Handing it back therefore
// cannot depend on the run: a release that inherited the cancelled context would
// fail on its first call, and the only report of that failure is a log line.
func TestAStandbyThatWinsInsideTheStopWindowStillHandsTheSlotBack(t *testing.T) {
	store := newStopWindowLocker()
	store.inner.deny = true
	value := mustNew(t, store, WithStandbyRetry(time.Millisecond))
	decision, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted, "the slot is held elsewhere, so this process starts as a standby")
	const key = "xbc:workload:sast:0"

	// From here the next acquisition is held inside the store, and would win.
	store.holdAcquisitions()
	store.inner.mu.Lock()
	store.inner.deny = false
	store.inner.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	store.awaitAcquisition(t)

	// The run is cancelled while the acquisition is in flight: the standby is
	// past every check it could have made, and the store is about to hand it a
	// slot regardless.
	host.requestStop()
	store.releaseAcquisitions()

	reason := host.shutdownReason(t)
	assert.Contains(t, reason, key, "a standby that won still asks for the restart that lets it host the slot")
	host.awaitTasks(t)

	assert.False(t, store.inner.holds(key), "a slot won inside the stop window is handed back, not left to expire")
	owed := value.releasable()
	require.Len(t, owed, 1, "the win is recorded before the handover, so a failed handover would still be owed")
	assert.True(t, owed[0].snapshot().released)
	assert.True(t, value.Stats().Standby, "the process hosts nothing new: it is restarting in order to host the slot")

	require.NoError(t, value.stop(context.Background()))
	assert.Equal(t, 1, store.releaseAttempts(), "a completed handover is not attempted again")
}

// TestARefusedStandbyHandoverIsRetriedByTheStopBackstop pins why the win is
// recorded rather than only released.
//
// A standby's handover is a warning and not a fatal error, so a store that
// refuses it leaves the process restarting with the slot still in its name. The
// slot never entered the held set, so unless the win is recorded, the Stop
// backstop walks an empty list and the capacity sits idle until its ttl expires
// -- which for a process that is restarting to take that very slot up means it
// may lose the race for it to nobody at all.
func TestARefusedStandbyHandoverIsRetriedByTheStopBackstop(t *testing.T) {
	store := newStopWindowLocker()
	store.inner.deny = true
	value := mustNew(t, store, WithStandbyRetry(time.Millisecond))
	decision, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted)
	const key = "xbc:workload:sast:0"

	// The store grants the win and then refuses the handover.
	store.inner.mu.Lock()
	store.inner.deny = false
	store.inner.releaseErr = errStoreUnreachable
	store.inner.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))

	reason := host.shutdownReason(t)
	assert.Contains(t, reason, key, "a refused handover is a warning, not a reason to stay put")
	host.awaitTasks(t)

	require.True(t, store.inner.holds(key), "the store refused the handover, so the slot is still in this process's name")
	owed := value.releasable()
	assert.Len(t, owed, 1, "the slot is still owed to the store")
	for _, slot := range owed {
		assert.False(t, slot.snapshot().released, "a refused handover is not recorded as done")
	}

	// The backstop is the second chance, exactly as it is for a held slot.
	store.inner.mu.Lock()
	store.inner.releaseErr = nil
	store.inner.mu.Unlock()

	require.NoError(t, value.stop(context.Background()))
	assert.Equal(t, 2, store.releaseAttempts(), "Stop attempts the handover the standby could not complete")
	assert.False(t, store.inner.holds(key))
	assert.True(t, value.Stats().Standby, "a slot won on the way out is not a workload this process hosts")
}

// TestPreStopWaitsForTheStandbyLoopToGoQuiet is the ordering half of the same
// defect.
//
// PreStop releases after the managed loop has gone quiet, so that the handover
// is not racing a store call that is already in flight. A standby loop is such a
// loop: its call wins slots rather than renewing them, which is the more
// expensive way to be racing a release. Waiting only for a renewal loop leaves
// the standby to be waited for by Stop instead -- after the phase that was
// designed to do the ordering has already finished.
func TestPreStopWaitsForTheStandbyLoopToGoQuiet(t *testing.T) {
	store := newStopWindowLocker()
	store.inner.deny = true
	value := mustNew(t, store, WithStandbyRetry(time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	store.holdAcquisitions()
	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	store.awaitAcquisition(t)
	host.requestStop()

	released := make(chan error, 1)
	go func() { released <- value.preStop(context.Background()) }()

	// This asserts the absence of a return, and an absence is only observable by
	// waiting longer than the work would take. The call being held cannot finish
	// until the test lets it, so a return here is a defect rather than a race.
	select {
	case err := <-released:
		t.Fatalf("PreStop released while the standby loop was still inside the lease store: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	store.releaseAcquisitions()

	select {
	case err := <-released:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("PreStop did not return once the standby loop had gone quiet")
	}
	host.awaitTasks(t)
}

// ── what a refused submission owes back ────────────────────────────────────

// TestARefusedLoopGivesBackItsDoneSignalAndItsRunContext covers the two
// cleanups a refused managed-task submission owes beyond the wait-group count
// that start_test.go pins.
//
// The done signal is observable through PreStop: the phase waits for a loop that
// was never admitted and, on a context with no deadline of its own, waits for
// ever. The run context is not reachable from outside start, so it is pinned at
// the function that owes it; the effect of missing it is a child context that
// stays attached to the plugin's context for the rest of the run, which no
// observation from outside can see and go vet's lostcancel cannot report because
// the cancel function is used inside the submitted closure.
func TestARefusedLoopGivesBackItsDoneSignalAndItsRunContext(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	host := newRefusingHost()
	require.Error(t, value.start(host.context()))

	released := make(chan error, 1)
	go func() { released <- value.preStop(context.Background()) }()
	select {
	case err := <-released:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("PreStop waited for a loop the runtime never admitted")
	}

	// abandonLoop is called with the wait-group count start took before
	// submitting, so the count is taken here the same way.
	runCtx, cancelRun := context.WithCancel(context.Background())
	value.loops.Add(1)
	value.abandonLoop(cancelRun)
	assert.ErrorIs(t, runCtx.Err(), context.Canceled, "a loop that was refused leaves no live context behind")
}
