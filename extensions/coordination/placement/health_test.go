package placement

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

// TestReadinessFollowsThisProcessesRenewals pins what the probe is allowed to
// answer. It reflects in-memory state only, so it never turns a diagnostics
// surface into load on the store whose availability is in question, and it
// reports this process's own claims rather than the store's reachability.
func TestReadinessFollowsThisProcessesRenewals(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)
	checks := newHealthProbe(value).HealthChecks()
	require.Len(t, checks, 1)
	assert.Equal(t, health.Readiness, checks[0].Kind)
	assert.Empty(t, checks[0].Name, "the check aggregates under the health Definition's own name")

	// A process that holds nothing is ready. That is the standby shape, and
	// reporting it down would make an orchestrator replace the very process
	// whose job is to be available to take over.
	require.NoError(t, checks[0].Checker.Check(context.Background()))

	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)
	require.NoError(t, checks[0].Checker.Check(context.Background()), "a slot that has just been won is confirmed")

	// A renewal the store does not confirm takes the probe down while the
	// process keeps working. That pairing is the honest report: this instance
	// has not stopped, and its claim on the capacity is no longer being
	// confirmed.
	locker.mu.Lock()
	locker.renewErr = errStoreUnreachable
	locker.mu.Unlock()
	value.renewHeld(context.Background())

	err = checks[0].Checker.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sast slot 0")
	assert.Contains(t, err.Error(), "not being confirmed")

	// It recovers as soon as a renewal is confirmed again, so a store blip does
	// not leave the instance reporting down for the rest of its life.
	locker.mu.Lock()
	locker.renewErr = nil
	locker.mu.Unlock()
	value.renewHeld(context.Background())
	require.NoError(t, checks[0].Checker.Check(context.Background()))
}

// TestReadinessStaysUpForAStandby is the other half of the same rule, stated
// separately because it is the case an operator is most likely to get wrong: a
// standby is a healthy process, not a broken one.
func TestReadinessStaysUpForAStandby(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker)
	_, err := value.Resolve(workloadRequest(ordinary("sast", 3)))
	require.NoError(t, err)

	checks := newHealthProbe(value).HealthChecks()
	require.NoError(t, checks[0].Checker.Check(context.Background()))
	assert.True(t, value.Stats().Standby)
}

// TestTheProbeIsExportedForTheHealthAggregator keeps the contract wiring
// checkable: the probe has to satisfy health.Contributor, because that is the
// type the graph binds the aggregator to.
func TestTheProbeIsExportedForTheHealthAggregator(t *testing.T) {
	var contributor health.Contributor = newHealthProbe(mustNew(t, newMemoryLocker()))
	assert.Len(t, contributor.HealthChecks(), 1)
}
