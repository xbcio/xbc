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
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
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
