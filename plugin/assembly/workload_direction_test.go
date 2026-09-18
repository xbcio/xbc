package assembly

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// directionContract is the interface every producer in this file exports. It is
// deliberately separate from coreContract so that a direction fixture cannot be
// satisfied by a producer some other test in this package happened to declare.
type directionContract interface{ Marker() string }

type directionValue struct{ marker string }

func (value *directionValue) Marker() string { return value.marker }

// directionSlot names where a Definition lives in a direction fixture. Two
// workloads exist so that "workload -> a different workload" has a second
// workload to point at, and so that the same-workload case -- a dispatcher
// requiring its own worker -- can be told apart from it.
type directionSlot string

const (
	slotUnowned directionSlot = "unowned"
	slotAlpha   directionSlot = "alpha"
	slotBeta    directionSlot = "beta"
)

const (
	directionProducerKey plugin.Key = "producer"
	directionConsumerKey plugin.Key = "consumer"
)

// directionQuery is one way a declaration can reach a producer. The four forms
// are why the guard cannot be written against the contract index alone: only
// three of them consult it, and RefTo reaches its target by key instead.
type directionQuery struct {
	name string
	// form is the phrase the diagnostic must use for this query, because an
	// operator fixing a by-type dependency edits the contract it asks for and
	// one fixing a by-name dependency edits the key it names.
	form    string
	declare func(producer plugin.Key) plugin.InputSet
}

var directionQueries = []directionQuery{
	{
		name: "RequireOne",
		form: "by type",
		declare: func(plugin.Key) plugin.InputSet {
			return plugin.Inputs(plugin.RequireOne[directionContract]())
		},
	},
	{
		name: "Collect",
		form: "by type",
		declare: func(plugin.Key) plugin.InputSet {
			return plugin.Inputs(plugin.Collect[directionContract]())
		},
	},
	{
		// OptionalOne is the second kind that tolerates finding nothing, so it
		// shares Collect's verdict on every row rather than RequireOne's --
		// which is the only reason the two allowed directions are two and not
		// one, and therefore has to be exercised as its own column.
		name: "OptionalOne",
		form: "by type",
		declare: func(plugin.Key) plugin.InputSet {
			return plugin.Inputs(plugin.OptionalOne[directionContract]())
		},
	},
	{
		name: "RefTo",
		form: "by name",
		declare: func(producer plugin.Key) plugin.InputSet {
			return plugin.Inputs(plugin.RefTo[directionContract](producer))
		},
	},
}

// directionBundle places one Definition in the slot a case asks for. An unowned
// Definition goes into a plain Bundle; a workload member goes into a
// WorkloadOf. That single difference is the whole of what placement can see.
func directionBundle(slot directionSlot, definition plugin.Definition) plugin.Bundle {
	if slot == slotUnowned {
		return plugin.BundleOf(definition)
	}
	return plugin.WorkloadOf(plugin.WorkloadKey(slot), plugin.BundleOf(definition))
}

// directionPlanIn builds a two-Definition composition -- one producer, one
// consumer, each in the requested slot -- and plans it under the given hosting
// decision. The producer's key is handed to declare so a Ref can name it.
//
// The planning path is the production one: real WorkloadOf Bundles, a real
// BuildPlan, and the same wiring every application goes through. A fixture that
// called the direction check directly would prove the check works and nothing
// about whether it is reachable.
func directionPlanIn(t *testing.T, placement plugin.Placement, producerSlot, consumerSlot directionSlot, declare func(plugin.Key) plugin.InputSet) (*Plan, error) {
	t.Helper()
	producer := plugin.Define(directionProducerKey, func(plugin.BuildContext) (*directionValue, error) {
		return &directionValue{marker: "producer"}, nil
	}, plugin.Options[*directionValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *directionValue) directionContract { return value })),
	})
	consumer := plugin.Define(directionConsumerKey, func(plugin.BuildContext) (*directionValue, error) {
		return &directionValue{marker: "consumer"}, nil
	}, plugin.Options[*directionValue]{Inputs: declare(directionProducerKey)})

	return BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{
			directionBundle(producerSlot, producer),
			directionBundle(consumerSlot, consumer),
		},
		Env:       testEnvironment(t, nil),
		Placement: placement,
	})
}

// directionPlan plans with the zero Placement, which hosts every declared
// workload. Cases about an unhosted workload pass a Placement explicitly.
func directionPlan(t *testing.T, producerSlot, consumerSlot directionSlot, declare func(plugin.Key) plugin.InputSet) (*Plan, error) {
	t.Helper()
	return directionPlanIn(t, plugin.Placement{}, producerSlot, consumerSlot, declare)
}

// directionConsumerIdentity is the consumer's key as the plan records it.
// InstanceInputs looks an identity up verbatim in the planned set, so the
// default instance has to be spelled out rather than left empty.
func directionConsumerIdentity() plugin.Identity {
	return plugin.Identity{Plugin: directionConsumerKey, Instance: plugin.DefaultInstance}
}

// directionContractName is the contract's name exactly as a diagnostic prints
// it: the declaration's own token type is reflect.TypeOf((*T)(nil)).Elem(), so
// building the expected string the same way is what keeps the assertion from
// drifting the moment the package or the type is renamed.
func directionContractName() string {
	return reflect.TypeOf((*directionContract)(nil)).Elem().String()
}

// TestDependencyDirectionFollowsPlacementNotQueryForm is the matrix: five
// placements of a consumer relative to its producer, crossed with the four
// ways a declaration can reach one.
//
// The matrix is the delivery boundary because the rule is about direction, not
// about query kind -- but the one place the two interact is the row that
// matters most. An unowned plugin may collect or optionally require from a
// workload but may not require exactly one from it, and that asymmetry is the
// entire reason the two allowed directions are allowed: a query that tolerates
// finding nothing cannot let the hosted set decide whether the process starts.
func TestDependencyDirectionFollowsPlacementNotQueryForm(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		producer directionSlot
		consumer directionSlot
		// verdict is the expected outcome per query name.
		verdict map[string]bool
	}{
		"workload depends on an unowned plugin": {
			producer: slotUnowned, consumer: slotAlpha,
			verdict: map[string]bool{"RequireOne": true, "Collect": true, "OptionalOne": true, "RefTo": true},
		},
		"workload depends on its own member": {
			producer: slotAlpha, consumer: slotAlpha,
			verdict: map[string]bool{"RequireOne": true, "Collect": true, "OptionalOne": true, "RefTo": true},
		},
		"unowned plugin depends on an unowned plugin": {
			producer: slotUnowned, consumer: slotUnowned,
			verdict: map[string]bool{"RequireOne": true, "Collect": true, "OptionalOne": true, "RefTo": true},
		},
		"unowned plugin depends on a workload": {
			producer: slotAlpha, consumer: slotUnowned,
			verdict: map[string]bool{"RequireOne": false, "Collect": true, "OptionalOne": true, "RefTo": false},
		},
		"workload depends on a different workload": {
			producer: slotBeta, consumer: slotAlpha,
			verdict: map[string]bool{"RequireOne": false, "Collect": false, "OptionalOne": false, "RefTo": false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, query := range directionQueries {
				t.Run(query.name, func(t *testing.T) {
					t.Parallel()
					plan, err := directionPlan(t, testCase.producer, testCase.consumer, query.declare)
					if !testCase.verdict[query.name] {
						require.Errorf(t, err, "%s must be refused", query.name)
						assert.Contains(t, err.Error(), "xbc: plugin consumer ",
							"the refusal names the consuming plugin")
						return
					}
					require.NoError(t, err)

					// Allowed has to mean wired, not merely tolerated: an
					// allowed direction that bound nothing would pass the
					// verdict above while silently dropping the dependency.
					// Every producer in this matrix is hosted, so a query that
					// tolerates zero candidates still has to bind the one that
					// is there.
					edges := plan.InstanceInputs(directionConsumerIdentity())
					require.Len(t, edges, 1)
					assert.Equal(t, []plugin.Identity{
						{Plugin: directionProducerKey, Instance: plugin.DefaultInstance},
					}, edges[0].Producers, "an allowed direction still binds its producer")
				})
			}
		})
	}
}

// TestDependencyDirectionStatesTheRuleAndNamesBothSides pins the diagnostics,
// not merely the verdicts. A test that only asserted "something failed" would
// pass on an unrelated error and would let the message rot into something an
// operator cannot act on.
func TestDependencyDirectionStatesTheRuleAndNamesBothSides(t *testing.T) {
	t.Parallel()
	contract := directionContractName()

	cases := map[string]struct {
		producer, consumer directionSlot
		query              string
		contains           []string
	}{
		"unowned plugin requiring from a workload, by type": {
			producer: slotAlpha, consumer: slotUnowned, query: "RequireOne",
			contains: []string{
				"xbc: plugin consumer requires exactly one " + contract + " by type from plugin producer in workload \"alpha\"",
				"an unowned plugin cannot depend on a workload-scoped producer by type",
				"placement rather than by the composition",
			},
		},
		"unowned plugin requiring from a workload, by name": {
			producer: slotAlpha, consumer: slotUnowned, query: "RefTo",
			contains: []string{
				"xbc: plugin consumer requires " + contract + " by name from plugin producer in workload \"alpha\"",
				"an unowned plugin cannot depend on a workload-scoped producer by name",
			},
		},
		"workload requiring from another workload, by type": {
			producer: slotBeta, consumer: slotAlpha, query: "RequireOne",
			contains: []string{
				"xbc: plugin consumer requires exactly one " + contract + " by type from plugin producer in workload \"beta\"",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
				"co-resident is decided by placement rather than by the graph",
			},
		},
		"workload collecting from another workload, by type": {
			producer: slotBeta, consumer: slotAlpha, query: "Collect",
			contains: []string{
				"xbc: plugin consumer collects " + contract + " by type from plugin producer in workload \"beta\"",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
			},
		},
		"workload optionally requiring from another workload, by type": {
			producer: slotBeta, consumer: slotAlpha, query: "OptionalOne",
			contains: []string{
				"xbc: plugin consumer optionally requires " + contract + " by type from plugin producer in workload \"beta\"",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
			},
		},
		"workload requiring from another workload, by name": {
			producer: slotBeta, consumer: slotAlpha, query: "RefTo",
			contains: []string{
				"xbc: plugin consumer requires " + contract + " by name from plugin producer in workload \"beta\"",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
			},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var declare func(plugin.Key) plugin.InputSet
			for _, query := range directionQueries {
				if query.name == testCase.query {
					declare = query.declare
				}
			}
			require.NotNil(t, declare, "the case names a query form that does not exist")

			_, err := directionPlan(t, testCase.producer, testCase.consumer, declare)
			require.Error(t, err)
			for _, fragment := range testCase.contains {
				assert.Contains(t, err.Error(), fragment)
			}
			assert.NotContains(t, err.Error(), "is ambiguous",
				"the refusal is the direction rule, not some other resolution failure")
			assert.True(t, strings.HasPrefix(err.Error(), "xbc: "), "the message keeps the house prefix")
			assert.False(t, strings.HasSuffix(strings.TrimSpace(err.Error()), "."),
				"the message states the rule without a terminating period")
			assert.Len(t, strings.Split(err.Error(), "\n"), 2,
				"two lines: what was asked for, then the rule that refuses it")
		})
	}
}

// TestDependencyDirectionClosesTheQueryRefBypass is the named regression for
// the one path a contract-index guard cannot see.
//
// RefTo never consults the contract index: it resolves its target by key
// straight out of the instance map. A guard written against the index -- the
// obvious place, because that is where every other resolution rule lives --
// would leave plugin.RefTo[T](key) as documented public API that walks around
// the rule entirely, and it would do so without failing any test that only
// exercised RequireOne.
//
// The two assertions are a matched pair on purpose. The refusal proves the
// guard reaches this path; the unowned twin proves the test is not passing
// because RefTo is broken for some unrelated reason.
func TestDependencyDirectionClosesTheQueryRefBypass(t *testing.T) {
	t.Parallel()
	ref := func(producer plugin.Key) plugin.InputSet {
		return plugin.Inputs(plugin.RefTo[directionContract](producer))
	}

	refused, err := directionPlan(t, slotAlpha, slotUnowned, ref)
	require.Error(t, err, "an unowned plugin must not reach a workload's producer by name either")
	assert.Nil(t, refused)
	assert.Contains(t, err.Error(), "by name",
		"the refusal names the query form, because the fix differs from the by-type one")
	assert.Contains(t, err.Error(), `workload "alpha"`)
	assert.Contains(t, err.Error(), "producer")
	assert.NotContains(t, err.Error(), "found none",
		"this must not be answered by the contract index, which RefTo never consults")

	allowed, err := directionPlan(t, slotUnowned, slotUnowned, ref)
	require.NoError(t, err, "the same query form is legal between two unowned plugins")
	edges := allowed.InstanceInputs(directionConsumerIdentity())
	require.Len(t, edges, 1)
	assert.Equal(t, []plugin.Identity{
		{Plugin: directionProducerKey, Instance: plugin.DefaultInstance},
	}, edges[0].Producers)
}

// TestDependencyDirectionNamesAPlacementAbsenceRatherThanAWiringOne covers the
// case the guard exists for at all.
//
// An unhosted workload's Definitions are not in the plan, so a query aimed at
// one finds nothing -- and "found none" is then true but useless, because the
// producer is not missing from the composition. It is missing from this
// process, by a deployment decision the operator can change. The cause and the
// symptom are different, so they get different messages.
func TestDependencyDirectionNamesAPlacementAbsenceRatherThanAWiringOne(t *testing.T) {
	t.Parallel()
	contract := directionContractName()
	// Only alpha is carried, so every producer placed in beta is absent for a
	// reason that has nothing to do with the declaration being read.
	onlyAlpha := plugin.Placement{Source: "lease", Hosted: []plugin.WorkloadKey{"alpha"}}

	cases := map[string]struct {
		producer, consumer directionSlot
		query              string
		contains           []string
	}{
		"unowned plugin requiring from an unhosted workload, by type": {
			producer: slotBeta, consumer: slotUnowned, query: "RequireOne",
			contains: []string{
				"xbc: plugin consumer requires exactly one " + contract + " by type from workload \"beta\", which this process does not carry",
				"an unowned plugin cannot depend on a workload-scoped producer by type",
			},
		},
		"unowned plugin requiring from an unhosted workload, by name": {
			producer: slotBeta, consumer: slotUnowned, query: "RefTo",
			contains: []string{
				"xbc: plugin consumer requires " + contract + " by name from workload \"beta\", which this process does not carry",
				"an unowned plugin cannot depend on a workload-scoped producer by name",
			},
		},
		"workload requiring from an unhosted workload, by type": {
			producer: slotBeta, consumer: slotAlpha, query: "RequireOne",
			contains: []string{
				"xbc: plugin consumer requires exactly one " + contract + " by type from workload \"beta\", which this process does not carry",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
			},
		},
		"workload requiring from an unhosted workload, by name": {
			producer: slotBeta, consumer: slotAlpha, query: "RefTo",
			contains: []string{
				"xbc: plugin consumer requires " + contract + " by name from workload \"beta\", which this process does not carry",
				"workload \"alpha\" cannot depend on workload \"beta\" directly",
			},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var declare func(plugin.Key) plugin.InputSet
			for _, query := range directionQueries {
				if query.name == testCase.query {
					declare = query.declare
				}
			}
			require.NotNil(t, declare)

			_, err := directionPlanIn(t, onlyAlpha, testCase.producer, testCase.consumer, declare)
			require.Error(t, err)
			for _, fragment := range testCase.contains {
				assert.Contains(t, err.Error(), fragment)
			}
			assert.NotContains(t, err.Error(), "found none",
				"the cause is placement, not an empty composition")
		})
	}
}

// TestDependencyDirectionLeavesCollectingFromAPlacementAbsenceAlone is the
// other half of the rule: collecting from a workload this process does not
// carry must stay legal and stay silent.
//
// web collecting RouteContributor and health collecting Contributor are the
// shapes this protects. Both are unowned plugins, both collect by contract, and
// both must produce a usable process when the workloads that would have
// contributed are absent -- which, under lease placement, is the ordinary
// standby.
func TestDependencyDirectionLeavesCollectingFromAPlacementAbsenceAlone(t *testing.T) {
	t.Parallel()
	collect := func(plugin.Key) plugin.InputSet {
		return plugin.Inputs(plugin.Collect[directionContract]())
	}

	plan, err := directionPlanIn(t,
		plugin.Placement{Source: "lease"},
		slotBeta, slotUnowned, collect)
	require.NoError(t, err, "an unowned plugin may collect from a workload it does not carry")

	edges := plan.InstanceInputs(directionConsumerIdentity())
	require.Len(t, edges, 1, "the edge is kept even when it bound nothing")
	assert.Empty(t, edges[0].Producers,
		"zero producers is the legal outcome, not a dropped dependency")
}

// TestDependencyDirectionLeavesOptionallyRequiringAPlacementAbsenceAlone is the
// OptionalOne twin of the case above, and it is a separate test because the two
// tolerated kinds do not degrade to the same thing.
//
// Collect hands its consumer an empty slice, which reads the same whether the
// contract has no exporter or its exporter lives in a workload this process
// does not carry. OptionalOne hands over the zero value and a false found flag
// instead, so the assertion has to reach past the plan and into construction:
// the observable outcome of a tolerated absence is a consumer that was built
// and saw nothing, not merely an edge with no producers.
func TestDependencyDirectionLeavesOptionallyRequiringAPlacementAbsenceAlone(t *testing.T) {
	t.Parallel()
	optional := plugin.OptionalOne[directionContract]()
	var (
		injected plugin.Entry[directionContract]
		found    bool
	)
	producer := plugin.Define(directionProducerKey, func(plugin.BuildContext) (*directionValue, error) {
		return &directionValue{marker: "producer"}, nil
	}, plugin.Options[*directionValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *directionValue) directionContract { return value })),
	})
	consumer := plugin.Define(directionConsumerKey, func(context plugin.BuildContext) (*directionValue, error) {
		injected, found = optional.Get(context)
		return &directionValue{marker: "consumer"}, nil
	}, plugin.Options[*directionValue]{Inputs: plugin.Inputs(optional)})

	// The producer is placed in beta and nothing is hosted, so the only
	// Definition that could answer the query is absent for a placement reason.
	plan, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{
			plugin.WorkloadOf(plugin.WorkloadKey(slotBeta), plugin.BundleOf(producer)),
			plugin.BundleOf(consumer),
		},
		Env:       testEnvironment(t, nil),
		Placement: plugin.Placement{Source: "lease"},
	})
	require.NoError(t, err, "an unowned plugin may optionally require from a workload it does not carry")

	edges := plan.InstanceInputs(directionConsumerIdentity())
	require.Len(t, edges, 1, "the edge is kept even when it bound nothing")
	assert.Empty(t, edges[0].Producers,
		"zero producers is the legal outcome, not a dropped dependency")

	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err, "a tolerated absence must still produce a running process")
	assert.False(t, found, "the consumer is told the producer is not here")
	assert.Equal(t, plugin.Entry[directionContract]{}, injected,
		"an unsatisfied optional injects the zero entry -- no value, no identity, no workload")
}

// TestDependencyDirectionExplainsARefThatIsMerelyUnwired keeps the placement
// explanation from swallowing the ordinary one. A Ref whose target is in this
// process and simply does not export the contract is a wiring mistake in the
// declaration, and its message must still say so -- otherwise every Ref typo
// would be blamed on placement.
func TestDependencyDirectionExplainsARefThatIsMerelyUnwired(t *testing.T) {
	t.Parallel()
	// The producer is unowned and present, but exports nothing the consumer
	// asks for, because the consumer asks by a key that names no contract
	// holder of its type.
	producer := plugin.Define("plain", func(plugin.BuildContext) (*directionValue, error) {
		return &directionValue{}, nil
	})
	consumer := plugin.Define("consumer", func(plugin.BuildContext) (*directionValue, error) {
		return &directionValue{}, nil
	}, plugin.Options[*directionValue]{
		Inputs: plugin.Inputs(plugin.RefTo[directionContract]("elsewhere")),
	})

	_, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(producer, consumer)},
		Env:     testEnvironment(t, nil),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact producer elsewhere")
	assert.Contains(t, err.Error(), "not enabled or does not export that contract")
	assert.NotContains(t, err.Error(), "placement",
		"nothing about this mistake is a placement decision")
}
