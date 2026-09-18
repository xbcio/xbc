package placement

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisstore "github.com/xbcio/xbc/extensions/storage/redis"
	"github.com/xbcio/xbc/plugin"
)

// ── renewal ────────────────────────────────────────────────────────────────

func TestHeldSlotsAreRenewedOnceTheTrafficGateOpens(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	host := newTestHost()
	require.NoError(t, value.start(host.context()))

	// The gate is still closed, so no externally visible work may have started
	// yet: a renewal is a write to a shared store that another process's
	// placement decision depends on.
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, locker.renewCount(key), "the renewal loop waits for the traffic gate")

	host.openGate()
	await(t, "the renewal loop to confirm the slot", func() bool { return locker.renewCount(key) >= 2 })

	host.requestStop()
	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)
}

// TestAFailedRenewalKeepsHostingAndNeverAsksToExit is the soft-placement rule.
// The alternative -- releasing the slot or exiting -- turns a blip in a store
// whose availability the contract deliberately does not promise into a
// cluster-wide reshuffle or a restart loop.
func TestAFailedRenewalKeepsHostingAndNeverAsksToExit(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	locker.mu.Lock()
	locker.renewErr = errStoreUnreachable
	locker.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))

	// The rule is asserted as soon as one renewal has failed, before waiting for
	// the rest. A regression that gives the slot back on a failed renewal stops
	// the slot from being renewed at all, so the counter the wait below is
	// watching freezes, and the rule would otherwise surface as that wait timing
	// out rather than as this assertion failing.
	await(t, "the first failed renewal", func() bool { return value.renewFailures.Load() >= 1 })
	assert.True(t, locker.holds(key), "a slot is kept when its renewal fails")

	await(t, "three failed renewals", func() bool { return value.renewFailures.Load() >= 3 })
	assert.True(t, locker.holds(key), "a slot is kept through repeated failed renewals")

	assert.Empty(t, host.stopCh, "a renewal failure never asks the process to exit")
	stats := value.Stats()
	assert.GreaterOrEqual(t, stats.RenewFailures, uint64(3))
	require.Len(t, stats.Held, 1)
	assert.True(t, stats.Held[0].Degraded)

	host.requestStop()
	require.NoError(t, value.preStop(context.Background()))
	assert.False(t, locker.holds(key), "a failed renewal still releases on the way out")
	host.awaitTasks(t)
}

// TestASlotLostElsewhereIsAlsoKept pins the second half of soft placement: the
// store saying "this is not yours any more" is not a reason to stop running the
// work this process is already running.
//
// What has to be pinned is that the process *keeps hosting*: the failure count
// and a quiet shutdown channel alone would also hold for a process that had
// given the slot back, because releasing a key that already belongs to someone
// else changes nothing on the store side. The observation is therefore the
// local state -- the slot stays in the held set, is not released, and is still
// being renewed -- plus the probe reporting the claim as unconfirmed.
func TestASlotLostElsewhereIsAlsoKept(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	await(t, "a confirmed renewal", func() bool { return locker.renewCount(key) >= 1 })

	// Another owner takes the key, which is what a lapsed renewal looks like
	// from this side.
	locker.mu.Lock()
	locker.held[key] = "someone-else"
	locker.mu.Unlock()

	await(t, "the renewal failure counter", func() bool { return value.renewFailures.Load() >= 1 })

	// The slot is still this process's to run, and still reported as held: a
	// lost claim is reported through the probe's Degraded flag rather than by
	// dropping the workload.
	stats := value.Stats()
	require.Len(t, stats.Held, 1, "a slot the store no longer confirms is still held by this process")
	assert.Equal(t, plugin.WorkloadKey("sast"), stats.Held[0].Workload)
	assert.True(t, stats.Held[0].Degraded, "the claim is not being confirmed, and the probe has to say so")
	assert.False(t, stats.Standby)

	held := value.snapshotHeld()
	require.Len(t, held, 1)
	assert.False(t, held[0].snapshot().released, "a slot lost elsewhere is kept, not handed back")

	// Renewal also continues, which is what distinguishes "kept" from "kept but
	// quietly given up on": a slot that had been released stops being renewed
	// altogether, so a later round reaching the store is the observable proof
	// that the process is still asking about the slot it kept.
	before := locker.renewCount(key)
	value.renewHeld(context.Background())
	assert.Greater(t, locker.renewCount(key), before, "this process keeps renewing a slot the store no longer confirms")

	assert.Empty(t, host.stopCh)

	host.requestStop()
	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)
}

// ── release on the way out ─────────────────────────────────────────────────

// TestPreStopReleasesAndALaterRenewalDoesNotWriteTheSlotBack is the test this
// piece of the design turns on.
//
// Asserting that PreStop called Release is not enough: the failure it guards
// against is a renewal that lands after the release and puts the key back, so
// the process would keep capacity it has already given away until the ttl
// expired. The instrumentation is the recording store, which sees the calls
// themselves, and the assertion is made after a renewal round is forced
// through a slot that has already been released.
func TestPreStopReleasesAndALaterRenewalDoesNotWriteTheSlotBack(t *testing.T) {
	server, client := newMiniredis(t)
	recorder := newRecordingLocker(newRedisLocker(t, client))
	value := mustNew(t, recorder, WithRenewInterval(10*time.Millisecond))

	decision, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Equal(t, []plugin.WorkloadKey{"sast"}, decision.Hosted)
	const key = "xbc:workload:sast:0"
	require.True(t, server.Exists(key), "the slot is held in the store, not only in memory")

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	await(t, "the renewal loop to be genuinely running", func() bool { return recorder.renewals(key) >= 2 })

	// The runtime cancels the execution context first and runs the lifecycle
	// hooks afterwards, which is the order this plugin's PreStop depends on.
	host.requestStop()
	require.NoError(t, value.preStop(context.Background()))

	assert.False(t, server.Exists(key), "PreStop hands the slot back before the process stops")

	// Force one renewal round after the release. This is the tick the design's
	// mutation removes the guard for.
	value.renewHeld(context.Background())

	assert.False(t, server.Exists(key), "the released slot must not reappear in the store")
	released, renewed := recorder.renewAfterRelease(key)
	assert.True(t, released, "the slot was released, so this assertion is about the guard and not about a missing release")
	assert.False(t, renewed, "no renewal may reach the store after the slot was released")
	host.awaitTasks(t)
}

// TestAReleaseIsNotUndoneByALockerWhoseRenewalReestablishes pins the effect
// rather than the call.
//
// The Redis backend's owner-checked renewal cannot recreate a deleted key, so
// the write-back this design guards against is invisible through it. This store
// models the backend for which it is visible -- a renewal that sets its key
// again when it finds none -- so the guard is proven to hold for the effect and
// not merely for the absence of a call.
func TestAReleaseIsNotUndoneByALockerWhoseRenewalReestablishes(t *testing.T) {
	locker := newMemoryLocker()
	locker.renewReestablishes = true
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))

	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	await(t, "a renewal round", func() bool { return locker.renewCount(key) >= 2 })

	host.requestStop()
	require.NoError(t, value.preStop(context.Background()))
	require.False(t, locker.holds(key))

	value.renewHeld(context.Background())

	assert.False(t, locker.holds(key), "a renewal after the release must not put the key back")
	host.awaitTasks(t)
}

// TestStopAlsoReleasesWhenThePreStopPhaseIsConfiguredAway covers
// xbc.pre_stop_timeout: 0s, where no PreStop hook runs at all. Without this
// backstop a clean stop would leave the slot to expire on its own ttl.
func TestStopAlsoReleasesWhenThePreStopPhaseIsConfiguredAway(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	require.True(t, locker.holds(key))

	host.requestStop()
	require.NoError(t, value.stop(context.Background()))

	assert.False(t, locker.holds(key))
	host.awaitTasks(t)
}

// TestReleasedSlotsAreReleasedOnlyOnce pins idempotence, because PreStop and
// Stop both run on the ordinary path and a second release would report a
// failure for a slot that is already correctly gone.
func TestReleasedSlotsAreReleasedOnlyOnce(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	host.requestStop()

	require.NoError(t, value.preStop(context.Background()))
	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)

	releases := 0
	locker.mu.Lock()
	for _, event := range locker.events {
		if event.kind == "release" {
			releases++
		}
	}
	locker.mu.Unlock()
	assert.Equal(t, 1, releases)
}

// ── standby ────────────────────────────────────────────────────────────────

// TestAStandbyWinsARestartRatherThanHosting pins the takeover mechanism. The
// process was assembled without the workload the slot belongs to, so it cannot
// take the slot up where it stands: it hands the slot straight back and asks to
// be restarted, which is the only point at which its role can change.
func TestAStandbyWinsARestartRatherThanHosting(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker, WithStandbyRetry(10*time.Millisecond))

	decision, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Empty(t, decision.Hosted)
	const key = "xbc:workload:sast:0"

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))

	// While every slot is held elsewhere a standby has nothing to ask for.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, host.stopCh, "a standby that keeps losing must not restart-loop")

	locker.mu.Lock()
	locker.deny = false
	locker.mu.Unlock()

	reason := host.shutdownReason(t)
	assert.Contains(t, reason, key)
	assert.False(t, locker.holds(key), "the standby hands the slot back and restarts instead of hosting it")
	assert.True(t, value.Stats().Standby, "the decided placement still reports the standby shape")

	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)
}

// TestAStandbyKeepsTryingWhileTheStoreIsUnreachable covers a store that goes
// away after the decision instead of before it. The cold-start rule fails a
// process that cannot read the store at all, but a standby already hosts its
// unowned plugins and is ready, so it keeps retrying rather than failing a
// process that is doing exactly what it is supposed to.
func TestAStandbyKeepsTryingWhileTheStoreIsUnreachable(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker, WithStandbyRetry(5*time.Millisecond))

	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	// The store goes away after the decision was taken.
	locker.mu.Lock()
	locker.deny = false
	locker.failAcquire = errStoreUnreachable
	locker.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	await(t, "the standby to retry past the failure", func() bool { return len(locker.attempts()) >= 3 })
	assert.Empty(t, host.stopCh, "an unreachable store never fails a standby")

	// And it picks the store back up without being restarted.
	locker.mu.Lock()
	locker.failAcquire = nil
	locker.mu.Unlock()

	reason := host.shutdownReason(t)
	assert.Contains(t, reason, "xbc:workload:sast:")

	require.NoError(t, value.stop(context.Background()))
	host.awaitTasks(t)
}

// ── convergence ────────────────────────────────────────────────────────────

// TestConcurrentProcessesConvergeOnExactlyTheDeclaredReplicas is the
// thundering-herd case. Nine processes race four slots of one workload: exactly
// four may win, they must win four different slots, and the other five must
// come out as standbys rather than as holders of a slot somebody else also
// holds.
func TestConcurrentProcessesConvergeOnExactlyTheDeclaredReplicas(t *testing.T) {
	_, client := newMiniredis(t)
	const (
		processes = 9
		replicas  = 4
	)

	type outcome struct {
		decision plugin.Placement
		stats    Stats
		err      error
	}
	outcomes := make([]outcome, processes)

	var wait sync.WaitGroup
	for index := 0; index < processes; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			// Each process builds its own store handle, its own generator and
			// its own placement, exactly as separate processes would.
			locker, err := redisstore.NewLocker(client)
			if err != nil {
				outcomes[index] = outcome{err: err}
				return
			}
			value, err := New(locker, WithKeyPrefix("converge"))
			if err != nil {
				outcomes[index] = outcome{err: err}
				return
			}
			decision, err := value.Resolve(instanceRequest(fmt.Sprintf("host-a-1758091200-%02d", index), ordinary("sast", replicas)))
			if err != nil {
				outcomes[index] = outcome{err: err}
				return
			}
			outcomes[index] = outcome{decision: decision, stats: value.Stats()}
		}(index)
	}
	wait.Wait()

	holders := 0
	slots := make(map[int]bool)
	owners := make(map[string]bool)
	standbys := 0
	for _, result := range outcomes {
		require.NoError(t, result.err)
		if len(result.decision.Hosted) == 0 {
			standbys++
			assert.True(t, result.stats.Standby)
			continue
		}
		holders++
		require.Len(t, result.stats.Held, 1)
		assert.False(t, slots[result.stats.Held[0].Slot], "two processes won slot %d", result.stats.Held[0].Slot)
		slots[result.stats.Held[0].Slot] = true
		assert.False(t, owners[result.decision.Holder], "two processes claim the same identity")
		owners[result.decision.Holder] = true
		assert.Equal(t, result.decision.Holder, result.stats.Instance,
			"the claimant a decision reports and the identity its stats carry are the same process")
	}

	assert.Equal(t, replicas, holders, "exactly the declared number of replicas may win")
	assert.Equal(t, processes-replicas, standbys)
}

// TestAFreedSlotIsTakenOverAfterItsTTLExpires covers the failover boundary: a
// holder that is killed stops renewing, and the slot becomes available again
// when its ttl runs out rather than when the store notices anything.
func TestAFreedSlotIsTakenOverAfterItsTTLExpires(t *testing.T) {
	server, client := newMiniredis(t)
	ttl := 500 * time.Millisecond
	first := mustNew(t, newRedisLocker(t, client), WithTTL(ttl), WithRenewInterval(100*time.Millisecond))

	decision, err := first.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.Equal(t, []plugin.WorkloadKey{"sast"}, decision.Hosted)

	// The holder dies without releasing anything.
	candidate := mustNew(t, newRedisLocker(t, client), WithTTL(ttl), WithRenewInterval(100*time.Millisecond))
	taken, err := candidate.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	assert.Empty(t, taken.Hosted, "the slot is still held while the dead holder's ttl has not run out")

	// A decision is final, so standing by is not retried by the same value: the
	// store has to be asked again by a process that starts after the ttl lapsed.
	server.FastForward(ttl + time.Second)

	successor := mustNew(t, newRedisLocker(t, client), WithTTL(ttl), WithRenewInterval(100*time.Millisecond))
	takeover, err := successor.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	assert.Equal(t, []plugin.WorkloadKey{"sast"}, takeover.Hosted)
}
