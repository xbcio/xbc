package placement

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestNewRejectsEveryUnusableConfiguration(t *testing.T) {
	locker := newMemoryLocker()
	for name, testCase := range map[string]struct {
		options []Option
		want    string
	}{
		"nil locker": {
			options: nil,
			want:    "requires a non-nil lease store",
		},
		"empty prefix": {
			options: []Option{WithKeyPrefix("")},
			want:    "key prefix must be non-empty",
		},
		"padded prefix": {
			options: []Option{WithKeyPrefix(" xbc:workload ")},
			want:    "no surrounding whitespace",
		},
		"renew interval below a millisecond": {
			options: []Option{WithRenewInterval(time.Microsecond)},
			want:    "renew interval must be at least 1ms",
		},
		"standby retry below a millisecond": {
			options: []Option{WithStandbyRetry(0)},
			want:    "standby retry must be at least 1ms",
		},
		"ttl below a millisecond": {
			options: []Option{WithTTL(time.Microsecond)},
			want:    "ttl must be at least 1ms",
		},
		"renewal window too long for the ttl": {
			options: []Option{WithTTL(10 * time.Second), WithRenewInterval(6 * time.Second)},
			want:    "must not exceed half of ttl",
		},
		"nil option": {
			options: []Option{nil},
			want:    "option 0 is nil",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := locker
			if name == "nil locker" {
				_, err := New(nil, testCase.options...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), testCase.want)
				return
			}
			_, err := New(store, testCase.options...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.want)
		})
	}
}

// TestTTLDefaultsToThreeRenewalWindows pins the relationship the two knobs have
// when only one of them is named: a slot must survive at least one missed
// renewal, and the default has to express that rather than pick two unrelated
// numbers.
func TestTTLDefaultsToThreeRenewalWindows(t *testing.T) {
	value := mustNew(t, newMemoryLocker(), WithRenewInterval(4*time.Second))
	assert.Equal(t, 12*time.Second, value.ttl)

	named := mustNew(t, newMemoryLocker(), WithTTL(20*time.Second))
	assert.Equal(t, 20*time.Second, named.ttl)

	both := mustNew(t, newMemoryLocker(), WithTTL(30*time.Second), WithRenewInterval(9*time.Second))
	assert.Equal(t, 30*time.Second, both.ttl)
	assert.Equal(t, 9*time.Second, both.renewEvery)
}

func TestSlotKeysAreBuiltFromThePrefixWorkloadAndIndex(t *testing.T) {
	defaulted := mustNew(t, newMemoryLocker())
	assert.Equal(t, "xbc:workload:sast:0", defaulted.slotKey("sast", 0))
	assert.Equal(t, "xbc:workload:web-scan:11", defaulted.slotKey("web-scan", 11))

	custom := mustNew(t, newMemoryLocker(), WithKeyPrefix("acme"))
	assert.Equal(t, "acme:workload:sast:2", custom.slotKey("sast", 2))
}

func TestResolveHostsOneSlotPerAdmittedWorkload(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(workloadRequest(ordinary("beta", 3), ordinary("alpha", 2)))
	require.NoError(t, err)

	assert.Equal(t, placementSourceLease, decision.Source)
	assert.Equal(t, []plugin.WorkloadKey{"alpha", "beta"}, decision.Hosted, "the hosted set is sorted by key")
	require.Len(t, locker.heldKeys(), 2, "one slot per workload, not one per replica")
	assert.Len(t, decision.Notes, 2)
	assert.NotEmpty(t, decision.Holder)
	assert.True(t, decision.Hosts("alpha"))
	assert.False(t, decision.Hosts("gamma"))
}

// TestExclusiveWorkloadIsAttemptedFirstAndStopsAcquisition pins both halves of
// the exclusive rule: an exclusive workload is tried before anything else, and
// winning it ends the round. Without the second half the process would carry a
// workload with process-wide side effects beside other workloads, which is
// exactly what WithExclusiveProcess promises cannot happen.
func TestExclusiveWorkloadIsAttemptedFirstAndStopsAcquisition(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(workloadRequest(ordinary("aardvark", 4), exclusive("zebra", 1)))
	require.NoError(t, err)

	assert.Equal(t, []plugin.WorkloadKey{"zebra"}, decision.Hosted)
	assert.Equal(t, []string{"xbc:workload:zebra:0"}, locker.attempts(),
		"the exclusive workload is attempted first, and nothing is attempted after it is won")
}

// TestAFailedExclusiveAttemptFallsThroughToOrdinaryWorkloads is the other side
// of that rule. The exclusive requirement is about the process that carries it,
// not about every process: a process that could not win the exclusive slot is
// an ordinary process, and leaving it idle would strand capacity.
func TestAFailedExclusiveAttemptFallsThroughToOrdinaryWorkloads(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)
	// The exclusive workload is already taken elsewhere.
	locker.held["xbc:workload:zebra:0"] = "someone-else"

	decision, err := value.Resolve(workloadRequest(ordinary("aardvark", 4), exclusive("zebra", 1)))
	require.NoError(t, err)

	assert.Equal(t, []plugin.WorkloadKey{"aardvark"}, decision.Hosted)
	attempts := locker.attempts()
	require.Len(t, attempts, 2)
	assert.Equal(t, "xbc:workload:zebra:0", attempts[0], "the exclusive workload is attempted first")
	assert.Contains(t, attempts[1], "xbc:workload:aardvark:", "the search then falls through to the ordinary workload")
}

// TestDisabledWorkloadIsNeverAttempted pins the hard veto. It is a deployment's
// statement that this process must not carry a role, and a slot that happens to
// be free does not override it.
func TestDisabledWorkloadIsNeverAttempted(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(plugin.PlacementRequest{
		Workloads: []plugin.Workload{ordinary("alpha", 2), ordinary("beta", 2)},
		Enabled:   func(key plugin.WorkloadKey) bool { return key != "beta" },
	})
	require.NoError(t, err)

	assert.Equal(t, []plugin.WorkloadKey{"alpha"}, decision.Hosted)
	for _, key := range locker.attempts() {
		assert.NotContains(t, key, "beta", "a workload configuration excludes must never be leased, even when a slot is free")
	}
	assert.Contains(t, decision.Notes[0], "disabled in this process's configuration")
}

func TestEverySlotHeldElsewhereMakesTheProcessAStandby(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker)

	decision, err := value.Resolve(workloadRequest(ordinary("sast", 3)))
	require.NoError(t, err)

	assert.Empty(t, decision.Hosted)
	assert.Empty(t, decision.Holder, "a process that holds nothing claims nothing")
	assert.Len(t, locker.attempts(), 3, "every slot of the workload is attempted before standing by")
	assert.Contains(t, decision.Notes[len(decision.Notes)-1], "starts as a standby")

	stats := value.Stats()
	assert.True(t, stats.Standby)
	assert.Empty(t, stats.Held)
}

// TestSlotSearchStartsAtARandomOffset is the convergence guard. A search that
// always started at index 0 would make every candidate race the same slot and
// lose the same way forever, so a freed slot would go to whichever process
// happened to wake up first rather than to a search that spreads over the set.
func TestSlotSearchStartsAtARandomOffset(t *testing.T) {
	const (
		rounds   = 40
		replicas = 8
	)
	locker := newMemoryLocker()
	locker.deny = true

	for round := 0; round < rounds; round++ {
		value := mustNew(t, locker)
		_, err := value.Resolve(workloadRequest(ordinary("offset", replicas)))
		require.NoError(t, err)
	}

	attempts := locker.attempts()
	require.Len(t, attempts, rounds*replicas, "every round attempts every slot exactly once")

	firsts := make(map[string]bool)
	for round := 0; round < rounds; round++ {
		key := attempts[round*replicas]
		index := key[len("xbc:workload:offset:"):]
		firsts[index] = true
	}
	assert.GreaterOrEqual(t, len(firsts), 3,
		"the starting index must vary across runs; %v means the search is not spreading", firsts)
}

// TestUnreachableStoreFailsResolveInsteadOfHostingEverything is the cold-start
// rule. A process that cannot read the store must not fall back to hosting
// every declared workload: the hosted set would then depend on store
// availability, and doctor output, startup validation, exclusivity and capacity
// would all be non-deterministic and fail open.
func TestUnreachableStoreFailsResolveInsteadOfHostingEverything(t *testing.T) {
	locker := newMemoryLocker()
	locker.failAcquire = errStoreUnreachable
	value := mustNew(t, locker)

	_, err := value.Resolve(workloadRequest(ordinary("sast", 3)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workload "sast"`)
	assert.Contains(t, err.Error(), "xbc:workload:sast:")
	assert.Contains(t, err.Error(), "connection refused", "the backend's own error stays in the chain")
	assert.Contains(t, err.Error(), "must not guess and host everything instead")
	assert.Empty(t, locker.heldKeys())
}

// TestAPartialWinIsGivenBackWhenTheStoreFailsMidDecision pins the cleanup path:
// a process that won a slot and then could not finish deciding must not keep
// the capacity, because it never hosts what that slot stands for.
func TestAPartialWinIsGivenBackWhenTheStoreFailsMidDecision(t *testing.T) {
	locker := newMemoryLocker()
	// The first acquisition (alpha) succeeds; the second (beta) fails.
	locker.failOnAttempt = 2
	value := mustNew(t, locker)

	_, err := value.Resolve(workloadRequest(ordinary("alpha", 1), ordinary("beta", 2)))
	require.Error(t, err)
	assert.Empty(t, locker.heldKeys(), "the slot won before the failure must be handed back")
}

// TestResolveIsDecidedOnce pins that a second ask is answered from the first
// decision. The runtime and the doctor command may both resolve, and a second
// round would leave the first round's slots held by nobody.
func TestResolveIsDecidedOnce(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	first, err := value.Resolve(workloadRequest(ordinary("sast", 2)))
	require.NoError(t, err)
	attempts := len(locker.attempts())

	second, err := value.Resolve(workloadRequest(ordinary("sast", 2)))
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Len(t, locker.attempts(), attempts, "a decided placement does not compete again")
}

func TestWorkloadWithoutAReplicaIsRejected(t *testing.T) {
	value := mustNew(t, newMemoryLocker())
	_, err := value.Resolve(workloadRequest(plugin.Workload{Key: "sast", Replicas: 0}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares 0 replicas")
}

// TestARepeatedWorkloadKeyIsRefusedWithoutTakingASlot pins the invariant that a
// workload occupies at most one slot per process.
//
// Keeping that invariant is this component's job rather than its caller's: a
// request that names one workload twice would otherwise be honoured twice, and
// the process would carry two replicas of a workload the composition declared
// once -- capacity taken from every other process for work nobody asked this
// one to do, and a replica count that disagrees with what the deployment
// believes it is running.
func TestARepeatedWorkloadKeyIsRefusedWithoutTakingASlot(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	_, err := value.Resolve(workloadRequest(ordinary("sast", 4), ordinary("sast", 4)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workload "sast"`)
	assert.Contains(t, err.Error(), "declared twice")
	assert.Contains(t, err.Error(), "at most one slot per process")

	assert.Empty(t, locker.attempts(), "a refused request must not touch the store at all")
	assert.Empty(t, locker.heldKeys())
	assert.False(t, value.resolved, "a refused request leaves the placement undecided rather than half-decided")
}

// TestDefinitionRefusesToRunWithoutAnInstalledPlacement pins the failure a
// composition gets when the Bundle reaches the graph without New. It has to be
// loud: the alternative -- a Definition that quietly does nothing -- would
// leave every slot unrenewed and unreleased with nothing to say so.
func TestDefinitionRefusesToRunWithoutAnInstalledPlacement(t *testing.T) {
	previous := installedPlacement.Swap(nil)
	t.Cleanup(func() { installedPlacement.Store(previous) })

	_, err := installed()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Placement was installed")
	assert.Contains(t, err.Error(), "placement.New")
}

// TestBundleCarriesBothDefinitions drives the invariant that makes the Bundle
// usable on its own: selecting it is what both keeps the slots alive and
// reports whether they are still confirmed, so a Bundle that reached only one
// of the two Definitions would silently drop half the capability.
func TestBundleCarriesBothDefinitions(t *testing.T) {
	keys := make(map[plugin.Key]bool)
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(bundle)) {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		require.True(t, ok)
		keys[plugin.Key(descriptor.Key)] = true
	}
	assert.Equal(t, map[plugin.Key]bool{Key: true, HealthKey: true}, keys)
}

func TestStatsReportsTheDecisionAndItsSlots(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(workloadRequest(ordinary("beta", 2), ordinary("alpha", 2)))
	require.NoError(t, err)
	require.Equal(t, []plugin.WorkloadKey{"alpha", "beta"}, decision.Hosted)

	stats := value.Stats()
	assert.Equal(t, placementSourceLease, stats.Source)
	assert.False(t, stats.Standby)
	assert.Zero(t, stats.RenewFailures)
	require.Len(t, stats.Held, 2)
	assert.Equal(t, plugin.WorkloadKey("alpha"), stats.Held[0].Workload)
	assert.Equal(t, plugin.WorkloadKey("beta"), stats.Held[1].Workload)
	for _, slot := range stats.Held {
		assert.Equal(t, value.slotKey(slot.Workload, slot.Slot), slot.Key)
		assert.NotEmpty(t, slot.Owner)
		assert.NotZero(t, slot.Age)
		assert.False(t, slot.Degraded)
	}
}

func TestResolveWithoutWorkloadsIsAStandby(t *testing.T) {
	value := mustNew(t, newMemoryLocker())
	decision, err := value.Resolve(plugin.PlacementRequest{})
	require.NoError(t, err)
	assert.Empty(t, decision.Hosted)
	assert.Contains(t, decision.Notes[len(decision.Notes)-1], "starts as a standby")
}
