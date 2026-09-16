package assembly

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

type inspectedContract interface{ Inspected() }

type soloContract interface{ Solo() }

// absentContract is exported by nothing on purpose: it is how an optional or
// collecting query that resolves to nothing is produced.
type absentContract interface{ Absent() }

type inspectedValue struct{}

func (*inspectedValue) Inspected() {}

type soloValue struct{}

func (*soloValue) Solo() {}

func inspectedProducer(key plugin.Key) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*inspectedValue, error) {
		return &inspectedValue{}, nil
	}, plugin.Options[*inspectedValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *inspectedValue) inspectedContract { return value })),
	})
}

func soloProducer() plugin.Definition {
	return plugin.Define("solo", func(plugin.BuildContext) (*soloValue, error) {
		return &soloValue{}, nil
	}, plugin.Options[*soloValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *soloValue) soloContract { return value })),
	})
}

// inspectionBundle builds the graph the inspection accessors are read against:
// one consumer declaring every query kind, including two that nothing can
// satisfy.
func inspectionBundle(t *testing.T) (plugin.Bundle, plugin.Identity) {
	t.Helper()
	ref := plugin.RefTo[inspectedContract]("producer-b")
	one := plugin.RequireOne[soloContract]()
	many := plugin.Collect[inspectedContract]()
	optionalAbsent := plugin.OptionalOne[absentContract]()
	manyAbsent := plugin.Collect[absentContract]()
	consumer := plugin.Define("consumer", func(plugin.BuildContext) (*int, error) {
		return new(int), nil
	}, plugin.Options[*int]{
		Inputs: plugin.Inputs(ref, one, many, optionalAbsent, manyAbsent),
	})
	bundle := plugin.BundleOf(consumer, inspectedProducer("producer-b"), inspectedProducer("producer-a"), soloProducer())
	return bundle, plugin.Identity{Plugin: "consumer", Instance: plugin.DefaultInstance}
}

// TestPlanReportsUnsatisfiedOptionalAndManyInputs is the reason this accessor
// exists. An optional or collecting query that matches nothing is legal, so it
// produces no error and no log line, and the resulting application is
// indistinguishable from a wired one until the missing behaviour is noticed in
// production. The plan therefore has to keep the edge and report it as having
// no producer, rather than omit it.
func TestPlanReportsUnsatisfiedOptionalAndManyInputs(t *testing.T) {
	t.Parallel()
	bundle, consumer := inspectionBundle(t)
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)

	edges := plan.InstanceInputs(consumer)
	require.Len(t, edges, 5, "every declared input is reported, satisfied or not")

	type unsatisfiedEdge struct {
		contract string
		query    InputQuery
	}
	var unsatisfied []unsatisfiedEdge
	for _, edge := range edges {
		if len(edge.Producers) == 0 {
			unsatisfied = append(unsatisfied, unsatisfiedEdge{contract: edge.Contract.String(), query: edge.Query})
		}
	}
	assert.Equal(t, []unsatisfiedEdge{
		{contract: "assembly.absentContract", query: QueryOptional},
		{contract: "assembly.absentContract", query: QueryMany},
	}, unsatisfied, "both queries nothing satisfies must survive as edges without producers")
}

// TestPlanReportsInputEdgesInDeclarationOrder pins the whole shape at once: the
// query kind an operator saw in the declaration, the contract, and who ended up
// bound to it.
func TestPlanReportsInputEdgesInDeclarationOrder(t *testing.T) {
	t.Parallel()
	bundle, consumer := inspectionBundle(t)
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)

	edges := plan.InstanceInputs(consumer)
	require.Len(t, edges, 5)
	assert.Equal(t, []InputQuery{QueryRef, QueryOne, QueryMany, QueryOptional, QueryMany},
		[]InputQuery{edges[0].Query, edges[1].Query, edges[2].Query, edges[3].Query, edges[4].Query},
		"inputs keep the order the declaration wrote them in")
	assert.Equal(t, []plugin.Identity{{Plugin: "producer-b", Instance: plugin.DefaultInstance}}, edges[0].Producers)
	assert.Equal(t, []plugin.Identity{{Plugin: "solo", Instance: plugin.DefaultInstance}}, edges[1].Producers)
	assert.Equal(t, []plugin.Identity{
		{Plugin: "producer-a", Instance: plugin.DefaultInstance},
		{Plugin: "producer-b", Instance: plugin.DefaultInstance},
	}, edges[2].Producers, "producers of one input are reported in canonical identity order")
	assert.Empty(t, edges[3].Producers)
	assert.Empty(t, edges[4].Producers)
}

// TestPlanInputEdgesAreDeterministicAcrossPlans guards the property a
// diagnostic depends on: the same Bundles must describe the same graph on every
// run, even though wiring walks Go maps.
func TestPlanInputEdgesAreDeterministicAcrossPlans(t *testing.T) {
	t.Parallel()
	bundle, consumer := inspectionBundle(t)
	first, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	expected := first.InstanceInputs(consumer)

	for range 20 {
		plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
		require.NoError(t, err)
		assert.Equal(t, expected, plan.InstanceInputs(consumer))
	}
}

// TestPlanInputEdgesAreCopiedPerCall keeps a diagnostic from being able to
// corrupt the frozen graph it is only reading.
func TestPlanInputEdgesAreCopiedPerCall(t *testing.T) {
	t.Parallel()
	bundle, consumer := inspectionBundle(t)
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)

	edges := plan.InstanceInputs(consumer)
	require.NotEmpty(t, edges[2].Producers)
	edges[2].Producers[0] = plugin.Identity{Plugin: "tampered"}

	assert.Equal(t, plugin.Identity{Plugin: "producer-a", Instance: plugin.DefaultInstance},
		plan.InstanceInputs(consumer)[2].Producers[0], "the plan must not share its binding slices")
}

// TestPlanReportsWhereEachInstanceWasSelected answers the question a
// Definition's own declaration site cannot: a Definition declares itself inside
// its own package, so only the Bundle that selected it explains why it is in
// this application's graph.
func TestPlanReportsWhereEachInstanceWasSelected(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("selected", func(plugin.BuildContext) (*inspectedValue, error) {
		return &inspectedValue{}, nil
	})
	aggregate := plugin.BundleOf(definition)
	explicit := plugin.BundleOf(definition)
	aggregateOrigin := pluginmodel.BundleEntries(pluginmodel.Bundle(aggregate))[0].Origin
	explicitOrigin := pluginmodel.BundleEntries(pluginmodel.Bundle(explicit))[0].Origin
	require.NotEqual(t, aggregateOrigin, explicitOrigin, "two BundleOf calls are two selection sites")

	identity := plugin.Identity{Plugin: "selected", Instance: plugin.DefaultInstance}
	plan, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{aggregate, explicit},
		Env:     testEnvironment(t, nil),
	})
	require.NoError(t, err)
	assert.Equal(t, aggregateOrigin, plan.InstanceSelectedAt(identity),
		"selecting the same Definition twice is expected usage; the site that introduced it wins")
	assert.Contains(t, plan.InstanceSelectedAt(identity), "plan_inspection_test.go",
		"the selection site is a source location the reader can open")

	reversed, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{explicit, aggregate},
		Env:     testEnvironment(t, nil),
	})
	require.NoError(t, err)
	assert.Equal(t, explicitOrigin, reversed.InstanceSelectedAt(identity),
		"the first selection encountered decides, not the lowest source line")
}

// TestPlanInspectionIsSafeOnAbsentIdentitiesAndNilPlans keeps the new accessors
// under the same rule as the existing ones: a diagnostic may call them before
// it knows whether planning produced anything.
func TestPlanInspectionIsSafeOnAbsentIdentitiesAndNilPlans(t *testing.T) {
	t.Parallel()
	bundle, consumer := inspectionBundle(t)
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)

	absent := plugin.Identity{Plugin: "not-selected", Instance: plugin.DefaultInstance}
	assert.Nil(t, plan.InstanceInputs(absent))
	assert.Empty(t, plan.InstanceSelectedAt(absent))

	var missing *Plan
	assert.Nil(t, missing.InstanceInputs(consumer))
	assert.Empty(t, missing.InstanceSelectedAt(consumer))
}
