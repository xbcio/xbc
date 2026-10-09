package placement

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

// TestReadinessToleratesABlipAndReportsASustainedLoss pins what the probe is
// allowed to answer. It reflects in-memory state only, so it never turns a
// diagnostics surface into load on the store whose availability is in question,
// and it reports this process's own claims rather than the store's reachability.
//
// The two halves of the timing are the review's point: one failed round is a
// store that did not answer, not a claim that is gone, and every holder shares
// that store, so reporting down on the first failure would empty the whole
// workload's rotation at once and send its traffic to standbys. What is worth
// reporting is a claim that has gone unconfirmed for longer than the ttl it was
// written with, because from that moment the store may legitimately have handed
// the slot to somebody else.
func TestReadinessToleratesABlipAndReportsASustainedLoss(t *testing.T) {
	locker := newMemoryLocker()
	// A workload's replicas share one store, which is what makes a blip on it a
	// fleet-wide event rather than a local one: every holder notices in the
	// same round. One process per admission is the model, so the holders are
	// separate Placements over the same locker.
	const holders = 3
	values := make([]*Placement, 0, holders)
	checks := make([]health.Contributor, 0, holders)
	for i := 0; i < holders; i++ {
		value := mustNew(t, locker, WithRenewInterval(5*time.Millisecond), WithTTL(40*time.Millisecond))
		_, err := value.Resolve(workloadRequest(ordinary("sast", holders)))
		require.NoError(t, err)
		// The keepalive begins with the decision, so it is stopped here: this
		// test drives the rounds itself, and a background ticker would confirm
		// the slots again behind it.
		value.quiesce()
		value.awaitLoopQuiet(context.Background())
		values = append(values, value)
		checks = append(checks, newHealthProbe(value))
	}

	first := checks[0].HealthChecks()
	require.Len(t, first, 1)
	assert.Equal(t, health.Readiness, first[0].Kind)
	assert.Empty(t, first[0].Name, "the check aggregates under the health Definition's own name")
	for _, check := range checks {
		require.NoError(t, check.HealthChecks()[0].Checker.Check(context.Background()),
			"a slot that has just been won is confirmed")
	}

	// The blip takes every holder unconfirmed at once, which is exactly the
	// shape the probe has to survive.
	locker.mu.Lock()
	locker.renewErr = errStoreUnreachable
	locker.mu.Unlock()
	for _, value := range values {
		value.renewHeld(context.Background())
	}
	for _, check := range checks {
		require.NoError(t, check.HealthChecks()[0].Checker.Check(context.Background()),
			"a renewal that failed inside the ttl is a store that did not answer, not a claim that is gone")
	}

	// It is not silent either: the state a store outage should page on is the
	// per-slot and cumulative one, which is what the metrics bridge exports.
	for _, value := range values {
		stats := value.Stats()
		require.Len(t, stats.Held, 1)
		assert.Truef(t, stats.Held[0].Degraded, "slot %d is degraded", stats.Held[0].Slot)
		assert.EqualValues(t, 1, stats.RenewFailures)
	}

	// Past the ttl the slot may already belong to somebody else, so a holder
	// may be a second copy of the workload rather than the one to route to.
	// Each holder is awaited on its own, because each holder has its own grace
	// period to elapse: they were created one after another, and a renewal that
	// succeeded inside the first holder's last round can belong to the third
	// only microseconds later -- milliseconds under load. Reading every holder
	// the instant the first crosses would assert on a later one still inside
	// its own ttl.
	for index, check := range checks {
		await(t, "the probe to report a claim unconfirmed for longer than its ttl", func() bool {
			return check.HealthChecks()[0].Checker.Check(context.Background()) != nil
		})
		err := check.HealthChecks()[0].Checker.Check(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("sast slot %d", values[index].Stats().Held[0].Slot))
		assert.Contains(t, err.Error(), "not being confirmed")
	}

	// A confirmed round ends it, so a store that comes back does not leave the
	// instances reporting down for the rest of their lives.
	locker.mu.Lock()
	locker.renewErr = nil
	locker.mu.Unlock()
	for index, value := range values {
		value.renewHeld(context.Background())
		require.NoError(t, checks[index].HealthChecks()[0].Checker.Check(context.Background()))
	}
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
