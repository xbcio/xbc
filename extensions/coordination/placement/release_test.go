package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseAttempts counts the releases this store was asked for, so a test can
// tell "the backstop tried the handover again" apart from "the slot was already
// considered given back".
func (l *memoryLocker) releaseAttempts(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempts := 0
	for _, event := range l.events {
		if event.kind == "release" && event.key == key {
			attempts++
		}
	}
	return attempts
}

// TestReleaseGivesBackWhatResolveWonWhenNoPluginOwnsIt covers the path the
// runtime takes when a command returns before construction -- doctor, a plan
// that fails to build, a stop during planning. Those paths have no PreStop and
// no Stop, so Release is the only thing that gives the slots back, and it has
// to work on a run that was already asked to stop.
func TestReleaseGivesBackWhatResolveWonWhenNoPluginOwnsIt(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	// One replica pins the slot index the key is asserted on: the search for a
	// slot within a larger replica set starts at a random index.
	_, err := value.Resolve(context.Background(), workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"
	require.True(t, locker.holds(key))

	// The run a release is taken on may already be stopping, and a release that
	// inherited that cancellation would fail on its first store call.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, value.Release(stopped))
	assert.False(t, locker.holds(key), "the slot is given back even on a cancelled run")
	assert.Empty(t, value.Stats().Held, "the process stops reporting a slot it no longer holds")
	assert.Equal(t, 1, locker.releaseAttempts(key))

	// Idempotent, and safe beside the plugin's own release: a later Stop finds
	// nothing left to give and does not touch the store again.
	require.NoError(t, value.Release(context.Background()))
	assert.Equal(t, 1, locker.releaseAttempts(key), "a second release costs nothing")
	require.NoError(t, value.stop(context.Background()))
	assert.Equal(t, 1, locker.releaseAttempts(key), "a later Stop releases nothing more")
}

// TestReleaseDoesNotTurnAHolderIntoAStandby pins what a process reports about
// itself after a pre-construction release. Standby means "won no slot", and a
// process that won a slot and gave it back holds none but did take the role --
// reporting it as a standby would misdescribe the very process the release was
// performed for.
func TestReleaseDoesNotTurnAHolderIntoAStandby(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(context.Background(), workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.False(t, value.Stats().Standby)

	require.NoError(t, value.Release(context.Background()))

	stats := value.Stats()
	assert.Empty(t, stats.Held, "the slot the release confirmed is no longer reported as held")
	assert.False(t, stats.Standby,
		"a process that won a slot and gave it back is not the standby shape")
}

// TestReleaseKeepsAStandbyAStandby is the other side of the same report: a
// process whose decision won nothing is the standby it always was, and a
// release of its on-the-way-out wins cannot make it read as a former holder.
func TestReleaseKeepsAStandbyAStandby(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker)
	_, err := value.Resolve(context.Background(), workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.True(t, value.Stats().Standby)

	require.NoError(t, value.Release(context.Background()))
	assert.True(t, value.Stats().Standby, "a standby stays a standby across a release")
}

// TestReleaseReportsASlotTheStoreDidNotConfirm covers the store that refuses
// the handover on a pre-construction path. There is no later hook to retry, so
// the error is the operator's only notice that the lease is left to expire --
// and the slot must stay owed rather than be dropped as if it were given back.
func TestReleaseReportsASlotTheStoreDidNotConfirm(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(context.Background(), workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"

	locker.mu.Lock()
	locker.releaseErr = errStoreUnreachable
	locker.mu.Unlock()

	err = value.Release(context.Background())
	require.Error(t, err, "a store that refuses the handover must be reported upward")
	assert.Contains(t, err.Error(), "release slot")
	assert.Contains(t, err.Error(), key)
	assert.True(t, locker.holds(key), "a failed release leaves the slot held")
	assert.Len(t, value.Stats().Held, 1, "the slot stays this process's to give back")
}

// TestAFailedReleaseIsReportedAndRetriedByTheStopBackstop covers the store that
// refuses the handover.
//
// PreStop is the phase the design releases in, and its error is the only thing
// that tells an operator the slot was not given back: swallowing it would leave
// the process holding capacity until the ttl expired with nothing in the logs
// to say why. The failed handover must also not be recorded as done -- the slot
// is still held, and Stop is the backstop that gives it a second chance.
func TestAFailedReleaseIsReportedAndRetriedByTheStopBackstop(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker, WithRenewInterval(10*time.Millisecond))
	_, err := value.Resolve(context.Background(), workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	const key = "xbc:workload:sast:0"
	require.True(t, locker.holds(key))

	locker.mu.Lock()
	locker.releaseErr = errStoreUnreachable
	locker.mu.Unlock()

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	host.requestStop()

	err = value.preStop(context.Background())
	require.Error(t, err, "a store that refuses the handover must be reported upward")
	assert.Contains(t, err.Error(), "release slot")
	assert.Contains(t, err.Error(), key)
	assert.Contains(t, err.Error(), "connection refused", "the backend's own error stays in the chain")

	assert.True(t, locker.holds(key), "a failed release leaves the slot held")
	held := value.snapshotHeld()
	require.Len(t, held, 1, "the slot is still this process's to give back")
	assert.False(t, held[0].snapshot().released, "a failed release is not recorded as released")
	assert.Equal(t, 1, locker.releaseAttempts(key))

	// The backstop runs on the ordinary path too, and it is the second chance a
	// failed handover gets.
	locker.mu.Lock()
	locker.releaseErr = nil
	locker.mu.Unlock()

	require.NoError(t, value.stop(context.Background()))
	assert.Equal(t, 2, locker.releaseAttempts(key), "Stop attempts the release PreStop could not complete")
	assert.False(t, locker.holds(key))
	host.awaitTasks(t)
}
