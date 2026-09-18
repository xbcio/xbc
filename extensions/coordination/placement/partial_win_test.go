package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here cover one shape in the two places it occurs: a round that wins
// a slot for one workload and then fails on the next. The slot is this process's
// to give back although it will never host it, and both of the things that can
// go wrong with that are invisible from outside -- a release taken on a context
// that is already cancelled fails on its first call, and a slot that is dropped
// afterwards is referenced from nowhere, so the shutdown backstop cannot see it
// and the capacity sits idle until its ttl expires.
//
// The two call sites are covered separately because they differ in exactly the
// way that makes the fix awkward: Resolve runs under p.mu and on a context that
// is never cancelled, while the standby loop holds no lock and runs on the run
// context, which the stop request cancels underneath it.

// ── Resolve ────────────────────────────────────────────────────────────────

// TestAPartialWinTheStoreAcceptsBackIsNotRecordedAsOwed pins the half of the
// disposal that keeps the cold-start contract observable.
//
// The recorded set is what is still owed, not everything that was won: a store
// that takes the slot back leaves nothing behind, so a failed cold start reports
// no claim and holds none. Recording every partial win unconditionally and
// leaving the backstop to sort it out would be simpler and would make the
// startup rule -- "a process that cannot read the store fails, and takes nothing
// with it" -- untrue for as long as the process lasted.
func TestAPartialWinTheStoreAcceptsBackIsNotRecordedAsOwed(t *testing.T) {
	locker := newMemoryLocker()
	// alpha is won on the first acquisition; beta fails on the second.
	locker.failOnAttempt = 2
	value := mustNew(t, locker)

	_, err := value.Resolve(workloadRequest(ordinary("alpha", 1), ordinary("beta", 1)))
	require.Error(t, err)

	assert.Empty(t, locker.heldKeys(), "the slot won before the failure is handed back")
	assert.Empty(t, value.releasable(), "a handover the store confirmed leaves nothing owed")
	assert.Empty(t, value.Stats().Held, "a slot that was given back is not a workload this process hosts")
}

// TestARefusedPartialWinFromResolveIsRetriedByTheStopBackstop is the other half:
// what happens when the store does not take the slot back.
//
// Resolve's failure is a hard error by design -- the hosted set has to be
// decided before the graph exists, so a process that cannot read the store must
// not guess -- and the release failure on the way out is only a log line. So
// unless the claim is recorded, it is referenced nowhere: the slot the process
// won is held in its name, nothing on any shutdown path knows about it, and it
// can only expire with its ttl.
//
// Recording it has to happen here rather than inside acquireAll, because Resolve
// holds p.mu across the whole decision and the recording needs that same mutex.
func TestARefusedPartialWinFromResolveIsRetriedByTheStopBackstop(t *testing.T) {
	locker := newMemoryLocker()
	// alpha is won on the first acquisition, beta fails on the second, and the
	// store then refuses to take alpha back.
	locker.failOnAttempt = 2
	locker.releaseErr = errStoreUnreachable
	value := mustNew(t, locker)

	_, err := value.Resolve(workloadRequest(ordinary("alpha", 1), ordinary("beta", 1)))

	// The startup semantics are unchanged: the failure is the store's, it names
	// the workload it could not decide, and it says why guessing is not an
	// option.
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workload "beta"`)
	assert.Contains(t, err.Error(), "cannot reach the lease store")
	assert.Contains(t, err.Error(), "must not guess and host everything instead")

	const key = "xbc:workload:alpha:0"
	require.True(t, locker.holds(key), "the store refused the handover, so the slot is still in this process's name")
	owed := value.releasable()
	require.Len(t, owed, 1, "a handover that did not complete is still owed")
	assert.Equal(t, key, owed[0].key)
	assert.False(t, owed[0].snapshot().released)
	assert.Empty(t, value.Stats().Held, "a slot on its way back is not a workload this process hosts")

	// The backstop is the second chance, exactly as it is for a held slot.
	locker.mu.Lock()
	locker.releaseErr = nil
	locker.mu.Unlock()

	require.NoError(t, value.stop(context.Background()))
	assert.False(t, locker.holds(key), "Stop retries the handover Resolve could not complete")
}

// ── the standby loop ───────────────────────────────────────────────────────

// TestAStandbyPartialWinInsideTheStopWindowIsGivenBackNotLeaked covers the
// standby call site, where the caller's context is the thing that goes wrong.
//
// The run context is cancelled the moment the process is asked to stop, and the
// loop can be inside the store when that happens: a store that consults the
// caller's context only before its round trip grants the slot anyway. A release
// that inherited that context would then fail on its first call -- with a log
// line as its only report -- so the slot would be left to expire even though the
// store was answering perfectly well.
func TestAStandbyPartialWinInsideTheStopWindowIsGivenBackNotLeaked(t *testing.T) {
	store := newStopWindowLocker()
	store.inner.deny = true
	value := mustNew(t, store, WithStandbyRetry(time.Millisecond))
	decision, err := value.Resolve(workloadRequest(ordinary("alpha", 1), ordinary("beta", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted, "both slots are held elsewhere, so this process starts as a standby")
	const key = "xbc:workload:alpha:0"

	// From here the next round wins alpha and then fails on beta: acquisitions
	// three and four, counting the two Resolve already made.
	store.holdAcquisitions()
	store.inner.mu.Lock()
	store.inner.deny = false
	store.inner.failOnAttempt = 4
	store.inner.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	store.awaitAcquisition(t)

	// The run is cancelled while the round's first acquisition is in flight: the
	// loop is past every check it could have made, and the store is about to
	// hand it alpha regardless.
	host.requestStop()
	store.releaseAcquisitions()
	host.awaitTasks(t)

	assert.False(t, store.inner.holds(key),
		"a slot won before the round failed is handed back on a context the stop request cannot cancel")
	assert.Equal(t, 1, store.releaseAttempts(), "the handover was attempted, not skipped")
	assert.Empty(t, value.releasable(), "a handover the store confirmed leaves nothing owed")
	assert.Empty(t, host.stopCh, "a round that failed is not a win, so it is no reason to restart")
	assert.Empty(t, value.Stats().Held, "the process hosts nothing it won on its way out")
	assert.True(t, value.Stats().Standby)

	require.NoError(t, value.stop(context.Background()))
	assert.Equal(t, 1, store.releaseAttempts(), "a completed handover is not attempted again")
}

// TestARefusedStandbyPartialWinIsRetriedByTheStopBackstop pins why the standby's
// partial win is recorded and not only released.
//
// A standby keeps retrying past a store failure by design, so this path is the
// one that repeats: every failed round that got as far as winning a slot would
// strand that slot, and nothing would ever look at it again. The claim therefore
// has to reach the owed list -- which this call site, unlike Resolve, can do
// through adoptHandback, because it holds no lock.
func TestARefusedStandbyPartialWinIsRetriedByTheStopBackstop(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker, WithStandbyRetry(time.Millisecond))
	decision, err := value.Resolve(workloadRequest(ordinary("alpha", 1), ordinary("beta", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted)
	const key = "xbc:workload:alpha:0"

	// The next round wins alpha, fails on beta, and is then refused the
	// handover.
	locker.mu.Lock()
	locker.deny = false
	locker.failOnAttempt = 4
	locker.releaseErr = errStoreUnreachable
	locker.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))

	await(t, "the refused partial win to be recorded as owed", func() bool {
		return len(value.releasable()) == 1
	})
	require.True(t, locker.holds(key), "the store refused the handover, so the slot is still in this process's name")
	for _, slot := range value.releasable() {
		assert.False(t, slot.snapshot().released, "a refused handover is not recorded as done")
	}
	assert.Empty(t, host.stopCh, "a round that failed is not a win, so it is no reason to restart")
	assert.Empty(t, value.Stats().Held, "a slot on its way back is not a workload this process hosts")
	assert.True(t, value.Stats().Standby)

	locker.mu.Lock()
	locker.releaseErr = nil
	locker.mu.Unlock()

	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)
	assert.False(t, locker.holds(key), "Stop retries the handover the standby could not complete")
}
