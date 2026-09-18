package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// workloadKey is a stable key a workload package would declare, used to prove
// WorkloadOf accepts a package-level constant rather than only a literal.
const workloadKey WorkloadKey = "sast"

func workloadFixture(key Key) Definition {
	return Define(key, func(BuildContext) (*definitionValue, error) {
		return &definitionValue{value: 1}, nil
	})
}

func TestWorkloadOfTagsEveryOccurrenceAndDeclaresItself(t *testing.T) {
	t.Parallel()
	dispatcher := workloadFixture("sast-dispatcher")
	worker := workloadFixture("sast-worker")

	bundle := WorkloadOf(workloadKey, BundleOf(dispatcher, worker),
		WithExclusiveProcess(), WithReplicas(3))

	assert.Equal(t, []Workload{{Key: "sast", Exclusive: true, Replicas: 3}}, BundleWorkloads(bundle))

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	require.Len(t, entries, 2)
	for _, entry := range entries {
		assert.Equal(t, WorkloadKey("sast"), entry.Workload, "every occurrence is tagged with the workload it belongs to")
	}
	assert.Contains(t, entries[0].Origin, "workload_test.go:", "tagging preserves the declaration origin")
}

// TestWorkloadOfDefaultsToASingleReplica pins the default the placement model
// depends on: a declared workload is carried by exactly one process unless its
// declaration says otherwise, so forgetting WithReplicas narrows placement
// rather than widening it.
func TestWorkloadOfDefaultsToASingleReplica(t *testing.T) {
	t.Parallel()
	bundle := WorkloadOf(workloadKey, BundleOf(workloadFixture("solo")))

	workloads := BundleWorkloads(bundle)
	require.Len(t, workloads, 1)
	assert.Equal(t, 1, workloads[0].Replicas)
	assert.False(t, workloads[0].Exclusive)
}

func TestWorkloadOfAcceptsAnEmptyBundle(t *testing.T) {
	t.Parallel()
	bundle := WorkloadOf(workloadKey, BundleOf())

	assert.Empty(t, pluginmodel.BundleEntries(pluginmodel.Bundle(bundle)))
	assert.Equal(t, []Workload{{Key: "sast", Replicas: 1}}, BundleWorkloads(bundle))
}

// TestWorkloadOfRejectsInvalidDeclarations collects the five declaration
// mistakes WorkloadOf is responsible for. Each must be rejected where it is
// written rather than at planning time, because a Bundle that silently
// describes nothing is harder to diagnose than a panic that names the value.
func TestWorkloadOfRejectsInvalidDeclarations(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		declare func()
		message string
	}{
		"empty key": {
			declare: func() { WorkloadOf("", BundleOf(workloadFixture("member"))) },
			message: "xbc: plugin.WorkloadOf workload key cannot be empty",
		},
		"uppercase key": {
			declare: func() { WorkloadOf("Sast", BundleOf(workloadFixture("member"))) },
			message: `xbc: plugin.WorkloadOf workload key "Sast" contains invalid character 'S', only lowercase letters, digits, underscores, and hyphens are allowed`,
		},
		"key with a path separator": {
			declare: func() { WorkloadOf("sast.worker", BundleOf(workloadFixture("member"))) },
			message: `xbc: plugin.WorkloadOf workload key "sast.worker" contains invalid character '.', only lowercase letters, digits, underscores, and hyphens are allowed`,
		},
		"non-positive replicas": {
			declare: func() { WithReplicas(0) },
			message: "xbc: plugin.WithReplicas requires a positive replica count, got 0; a workload with no replica can never be carried",
		},
		"negative replicas": {
			declare: func() { WithReplicas(-2) },
			message: "xbc: plugin.WithReplicas requires a positive replica count, got -2; a workload with no replica can never be carried",
		},
		"nested workload with tagged members": {
			declare: func() {
				WorkloadOf("outer", WorkloadOf("inner", BundleOf(workloadFixture("inner-member"))))
			},
			message: `xbc: plugin.WorkloadOf for workload "outer" was given a Bundle whose occurrences already belong to workload "inner"; a Definition belongs to at most one workload`,
		},
		"nested empty workload": {
			declare: func() {
				WorkloadOf("outer", WorkloadOf("inner", BundleOf()))
			},
			message: `xbc: plugin.WorkloadOf for workload "outer" was given a Bundle that declares workload "inner"; compose Definitions here and select the nested workload at the composition root`,
		},
		"nil option": {
			declare: func() { WorkloadOf(workloadKey, BundleOf(workloadFixture("member")), nil) },
			message: "xbc: plugin.WorkloadOf item 0 is nil",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.PanicsWithValue(t, testCase.message, testCase.declare)
		})
	}
}

// TestWorkloadOwnershipSurvivesCombining is the delivery boundary for this
// step: a workload Bundle is consumed exactly like an ordinary one, so
// combining must carry both the membership of each occurrence and the
// declaration that explains it.
func TestWorkloadOwnershipSurvivesCombining(t *testing.T) {
	t.Parallel()
	member := workloadFixture("sast-worker")
	unowned := workloadFixture("web")

	workload := WorkloadOf(workloadKey, BundleOf(member), WithReplicas(2))
	ordinary := BundleOf(unowned)

	combined := CombineBundles(ordinary, workload)
	assert.Equal(t, []Workload{{Key: "sast", Replicas: 2}}, BundleWorkloads(combined),
		"the declaration travels with the occurrences that name it")

	byKey := make(map[Key]WorkloadKey)
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(combined)) {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		require.True(t, ok)
		byKey[descriptor.Key] = entry.Workload
	}
	assert.Equal(t, WorkloadKey("sast"), byKey["sast-worker"])
	assert.Equal(t, WorkloadKey(""), byKey["web"], "an unowned Definition reports no workload rather than an empty-named one")

	assert.Empty(t, BundleWorkloads(ordinary), "an ordinary Bundle declares no workload")
	assert.Nil(t, BundleWorkloads(Bundle{}))
}

// TestBundleWorkloadsIsDefensiveAndDeterministic pins the two properties
// callers rely on when reading declarations out of a composition they did not
// build: the result is sorted, and mutating it cannot reach the Bundle.
func TestBundleWorkloadsIsDefensiveAndDeterministic(t *testing.T) {
	t.Parallel()
	zulu := WorkloadOf("zulu", BundleOf(workloadFixture("zulu-member")))
	alpha := WorkloadOf("alpha", BundleOf(workloadFixture("alpha-member")))

	combined := CombineBundles(zulu, alpha)
	assert.Equal(t, []WorkloadKey{"alpha", "zulu"}, []WorkloadKey{
		BundleWorkloads(combined)[0].Key,
		BundleWorkloads(combined)[1].Key,
	})

	workloads := BundleWorkloads(combined)
	workloads[0].Replicas = 99
	assert.Equal(t, 1, BundleWorkloads(combined)[0].Replicas, "BundleWorkloads returns a defensive copy")
}

// TestWorkloadOfDoesNotAliasTheGivenBundle keeps workload tagging free of the
// aliasing that would let one composition's membership leak into another's.
func TestWorkloadOfDoesNotAliasTheGivenBundle(t *testing.T) {
	t.Parallel()
	base := BundleOf(workloadFixture("member"))
	workload := WorkloadOf(workloadKey, base)

	assert.Equal(t, WorkloadKey(""), pluginmodel.BundleEntries(pluginmodel.Bundle(base))[0].Workload,
		"tagging returns a new Bundle rather than mutating the one given")
	assert.Equal(t, WorkloadKey("sast"), pluginmodel.BundleEntries(pluginmodel.Bundle(workload))[0].Workload)
}

// TestOptionsCarryWorkloadAsAnEscapeHatch covers the member whose Definition
// lives in another module and therefore cannot be gathered by WorkloadOf: the
// ownership has to reach the descriptor so assembly can honour it.
func TestOptionsCarryWorkloadAsAnEscapeHatch(t *testing.T) {
	t.Parallel()
	definition := Define("remote-member", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{}, nil
	}, Options[*definitionValue]{Workload: workloadKey})

	assert.Equal(t, pluginmodel.WorkloadKey("sast"), descriptorOf(t, definition).Workload)

	unowned := Define("unowned", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{}, nil
	})
	assert.Equal(t, pluginmodel.WorkloadKey(""), descriptorOf(t, unowned).Workload,
		"a Definition that declares nothing belongs to no workload")
}

// TestEntryReportsTheProducingOccurrence (below) drives the read path directly,
// because plugin/assembly is what ultimately fills a slot and it is wired in a
// later step. The erased ResolvedEntry the slot holds is the contract between
// the two, so pinning it here is what makes that wiring mechanical.
func TestEntryReportsTheProducingOccurrence(t *testing.T) {
	t.Parallel()
	owned := RefTo[definitionContract]("producer")
	unowned := RefTo[definitionContract]("unowned")

	context := buildContextFor(t, Identity{Plugin: "consumer"}, map[uint64][]pluginmodel.ResolvedEntry{
		owned.inputToken().ID: {{
			Identity: pluginmodel.Identity{Plugin: "producer"},
			Value:    &definitionValue{value: 7},
			Workload: "sast",
		}},
		unowned.inputToken().ID: {{
			Identity: pluginmodel.Identity{Plugin: "unowned"},
			Value:    &definitionValue{value: 8},
		}},
	})

	entry := owned.Get(context)
	assert.Equal(t, WorkloadKey("sast"), entry.Workload)
	assert.Equal(t, 7, entry.Value.Value())

	assert.Equal(t, WorkloadKey(""), unowned.Get(context).Workload,
		"an unowned producer reports no workload rather than a zero-named one")
}

// TestCollectReportsWorkloadPerEntry is the shape a per-workload resource
// budget reads: one collected slice may span several workloads, so attribution
// has to be per entry rather than per query.
func TestCollectReportsWorkloadPerEntry(t *testing.T) {
	t.Parallel()
	many := Collect[definitionContract]()
	context := buildContextFor(t, Identity{Plugin: "consumer"}, map[uint64][]pluginmodel.ResolvedEntry{
		many.inputToken().ID: {
			{Identity: pluginmodel.Identity{Plugin: "shared"}, Value: &definitionValue{value: 1}},
			{Identity: pluginmodel.Identity{Plugin: "worker"}, Value: &definitionValue{value: 2}, Workload: "sast"},
		},
	})

	collected := many.Get(context)
	require.Len(t, collected, 2)
	assert.Equal(t, WorkloadKey(""), collected[0].Workload)
	assert.Equal(t, WorkloadKey("sast"), collected[1].Workload)
}
