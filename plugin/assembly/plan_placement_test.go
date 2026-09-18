package assembly

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
)

// placementFixtures is the composition every test in this file is read against:
// two workloads and one unowned Definition. The unowned one is what makes the
// hosted set a set rather than a switch -- it must be present in all three
// shapes, including the one that carries no workload at all, which is the
// standby a lease deployment falls back to.
type placementFixtures struct {
	bundles []plugin.Bundle
	calls   map[plugin.Key]*atomic.Int32
	// definitions is the Definition each keyed counter counts.
	definitions map[plugin.Key]plugin.Definition
}

func newPlacementFixtures() *placementFixtures {
	fixtures := &placementFixtures{
		calls:       make(map[plugin.Key]*atomic.Int32),
		definitions: make(map[plugin.Key]plugin.Definition),
	}
	define := func(key plugin.Key) plugin.Definition {
		calls := new(atomic.Int32)
		definition := plugin.Define(key, func(plugin.BuildContext) (*validationValue, error) {
			calls.Add(1)
			return &validationValue{}, nil
		})
		fixtures.calls[key] = calls
		fixtures.definitions[key] = definition
		return definition
	}
	fixtures.bundles = []plugin.Bundle{
		plugin.WorkloadOf("sast",
			plugin.BundleOf(define("sast-worker")),
			plugin.WithReplicas(3),
			plugin.WithExclusiveProcess(),
		),
		plugin.WorkloadOf("sca",
			plugin.BundleOf(define("sca-worker"), define("sca-api")),
			plugin.WithReplicas(2),
		),
		// The unowned Definition: transport and infrastructure plugins belong
		// to no workload and are therefore in every process shape.
		plugin.BundleOf(define("web")),
	}
	return fixtures
}

func (f *placementFixtures) constructed(t *testing.T, placement plugin.Placement) *Plan {
	t.Helper()
	plan, err := BuildPlan(PlanOptions{
		Bundles:   f.bundles,
		Env:       testEnvironment(t, nil),
		Placement: placement,
	})
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	return plan
}

func (f *placementFixtures) callCount(key plugin.Key) int32 { return f.calls[key].Load() }

// TestPlacementDecidesWhichDefinitionsEnterTheGraph is this task's delivery
// boundary for the assembled graph: a workload this process does not carry
// contributes no Definition, so its factories are never invoked and its
// identities never appear in the plan's order.
//
// The three shapes are one table because the interesting property is the middle
// one -- a partial hosted set -- and because "none" is the shape a lease
// deployment's standby takes, which must still carry every unowned Definition
// and still report ready.
func TestPlacementDecidesWhichDefinitionsEnterTheGraph(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		placement  plugin.Placement
		wantOrder  []plugin.Identity
		wantHosted map[plugin.WorkloadKey]bool
	}{
		"all workloads hosted": {
			placement: plugin.Placement{Source: "static", Hosted: []plugin.WorkloadKey{"sast", "sca"}},
			wantOrder: []plugin.Identity{
				{Plugin: "sast-worker", Instance: plugin.DefaultInstance},
				{Plugin: "sca-api", Instance: plugin.DefaultInstance},
				{Plugin: "sca-worker", Instance: plugin.DefaultInstance},
				{Plugin: "web", Instance: plugin.DefaultInstance},
			},
			wantHosted: map[plugin.WorkloadKey]bool{"sast": true, "sca": true},
		},
		"only sca hosted": {
			placement: plugin.Placement{Source: "lease", Hosted: []plugin.WorkloadKey{"sca"}},
			wantOrder: []plugin.Identity{
				{Plugin: "sca-api", Instance: plugin.DefaultInstance},
				{Plugin: "sca-worker", Instance: plugin.DefaultInstance},
				{Plugin: "web", Instance: plugin.DefaultInstance},
			},
			wantHosted: map[plugin.WorkloadKey]bool{"sast": false, "sca": true},
		},
		"no workload hosted": {
			placement: plugin.Placement{Source: "lease", Hosted: nil},
			wantOrder: []plugin.Identity{
				{Plugin: "web", Instance: plugin.DefaultInstance},
			},
			wantHosted: map[plugin.WorkloadKey]bool{"sast": false, "sca": false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixtures := newPlacementFixtures()
			plan := fixtures.constructed(t, testCase.placement)

			assert.Equal(t, testCase.wantOrder, plan.Order(),
				"an unhosted workload's identities must be absent, not merely disabled")

			for key, hosted := range testCase.wantHosted {
				if hosted {
					assert.Positive(t, fixtures.callCount(plugin.Key(key)+"-worker"),
						"a hosted workload's factory must have run")
					continue
				}
				assert.Zero(t, fixtures.callCount(plugin.Key(key)+"-worker"),
					"an unhosted workload's factory must never be invoked")
			}
			assert.Positive(t, fixtures.callCount("web"),
				"an unowned Definition belongs to every process shape")

			for _, workload := range plan.Workloads() {
				assert.Equal(t, testCase.wantHosted[workload.Workload.Key], workload.Hosted,
					"workload %s hosting decision", workload.Workload.Key)
			}
		})
	}
}

// TestPlanWorkloadsReportsTheWholeDeclaredSetIncludingWhatItLeftOut pins the
// shape doctor reads. An unhosted workload must be reported rather than
// omitted: "this process does not carry sast" is the answer an operator is
// looking for, and a workload missing from the list could not be told apart
// from one nothing declared.
func TestPlanWorkloadsReportsTheWholeDeclaredSetIncludingWhatItLeftOut(t *testing.T) {
	t.Parallel()
	fixtures := newPlacementFixtures()
	plan := fixtures.constructed(t, plugin.Placement{
		Source: "lease",
		Holder: "host-7-1726-9f3c1a2b",
		Hosted: []plugin.WorkloadKey{"sca"},
	})

	workloads := plan.Workloads()
	require.Len(t, workloads, 2, "both declared workloads are reported, hosted or not")

	sast := workloads[0]
	assert.Equal(t, plugin.WorkloadKey("sast"), sast.Workload.Key,
		"the reported set is sorted by key")
	assert.False(t, sast.Hosted)
	assert.True(t, sast.Workload.Exclusive, "the declaration's constraints are reported with it")
	assert.Equal(t, 3, sast.Workload.Replicas)
	assert.Empty(t, sast.Identities, "an unhosted workload contributed no identity")

	sca := workloads[1]
	assert.Equal(t, plugin.WorkloadKey("sca"), sca.Workload.Key)
	assert.True(t, sca.Hosted)
	assert.False(t, sca.Workload.Exclusive)
	assert.Equal(t, 2, sca.Workload.Replicas)
	assert.Equal(t, []plugin.Identity{
		{Plugin: "sca-api", Instance: plugin.DefaultInstance},
		{Plugin: "sca-worker", Instance: plugin.DefaultInstance},
	}, sca.Identities, "a hosted workload's identities are reported in graph order")

	placement := plan.Placement()
	assert.Equal(t, "lease", placement.Source)
	assert.Equal(t, "host-7-1726-9f3c1a2b", placement.Holder)
	assert.Equal(t, []plugin.WorkloadKey{"sca"}, placement.Hosted)

	// The attribution table a per-workload resource budget reads.
	scaWorker, ok := plan.WorkloadOf(plugin.Identity{Plugin: "sca-worker", Instance: plugin.DefaultInstance})
	require.True(t, ok)
	assert.Equal(t, plugin.WorkloadKey("sca"), scaWorker)

	web, ok := plan.WorkloadOf(plugin.Identity{Plugin: "web", Instance: plugin.DefaultInstance})
	assert.False(t, ok, "an unowned Definition belongs to no workload")
	assert.Empty(t, web)

	_, ok = plan.WorkloadOf(plugin.Identity{Plugin: "sast-worker", Instance: plugin.DefaultInstance})
	assert.False(t, ok, "an identity that is not in the plan has no workload")
}

// TestPlacementZeroValueHostsEverythingTheConfigurationEnables is what keeps
// every caller that has no placement source -- a test, a diagnostic, an
// embedding host -- planning exactly as it did before placement existed.
func TestPlacementZeroValueHostsEverythingTheConfigurationEnables(t *testing.T) {
	t.Parallel()
	fixtures := newPlacementFixtures()

	zero, err := BuildPlan(PlanOptions{Bundles: fixtures.bundles, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	static, err := BuildPlan(PlanOptions{
		Bundles:   fixtures.bundles,
		Env:       testEnvironment(t, nil),
		Placement: plugin.Placement{Source: "static", Hosted: []plugin.WorkloadKey{"sast", "sca"}},
	})
	require.NoError(t, err)

	assert.Equal(t, static.Order(), zero.Order(),
		"the zero Placement hosts exactly what StaticPlacement hosts")
	for _, workload := range zero.Workloads() {
		assert.True(t, workload.Hosted, "workload %s", workload.Workload.Key)
	}
	assert.NotEmpty(t, zero.Placement().Source,
		"a plan that made a decision must name the source that made it")
}

// TestPlacementLayersWithPluginEnablementBothWays pins the order of the three
// decisions that turn a declared Definition into an instance. The hosted set
// runs first and removes a Definition from consideration entirely; activation
// and "plugins.<key>.enabled" then decide what happens to the Definitions that
// remain. Both are applied and both are reported, and only the second and third
// are reported as disablements.
func TestPlacementLayersWithPluginEnablementBothWays(t *testing.T) {
	t.Parallel()
	configured := plugin.Define("configured", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Activation: plugin.WhenConfigured("plugins.gated")})
	plain := plugin.Define("plain", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	bundles := []plugin.Bundle{
		plugin.WorkloadOf("carried", plugin.BundleOf(configured, plain)),
	}

	t.Run("hosted workload, plugin turned off", func(t *testing.T) {
		t.Parallel()
		plan, err := BuildPlan(PlanOptions{
			Bundles: bundles,
			Env: testEnvironment(t, map[string]any{"plugins": map[string]any{
				"gated": map[string]any{"present": true},
				"plain": map[string]any{"enabled": false},
			}}),
			Placement: plugin.Placement{Source: "static", Hosted: []plugin.WorkloadKey{"carried"}},
		})
		require.NoError(t, err)
		assert.Equal(t, []plugin.Key{"plain"}, plan.Disabled(),
			"a hosted workload whose plugin section is off is an ordinary disabled plugin")
		assert.Equal(t, []plugin.Identity{
			{Plugin: "configured", Instance: plugin.DefaultInstance},
		}, plan.Workloads()[0].Identities,
			"the workload is still hosted, and still carries the instance that survived")
	})

	t.Run("unhosted workload, plugin turned on", func(t *testing.T) {
		t.Parallel()
		plan, err := BuildPlan(PlanOptions{
			Bundles:   bundles,
			Env:       testEnvironment(t, map[string]any{"plugins": map[string]any{"gated": map[string]any{"present": true}}}),
			Placement: plugin.Placement{Source: "lease", Hosted: nil},
		})
		require.NoError(t, err)
		assert.Empty(t, plan.Order())
		assert.Empty(t, plan.Disabled(),
			"an unhosted workload is invisible below Plan.Workloads, not reported as disabled")
		assert.Equal(t, 0, plan.DefinitionCount(),
			"a Definition this process does not have is not counted among the declared ones")
		assert.False(t, plan.Workloads()[0].Hosted)
	})
}

// TestReadWorkloadSectionsBindsEachWorkloadsOwnSection covers the reader the
// hosted set and the per-workload budget share. Every declared workload is
// reported, including one whose section is absent: absence means enabled, the
// same way it does for a plugin section.
func TestReadWorkloadSectionsBindsEachWorkloadsOwnSection(t *testing.T) {
	t.Parallel()
	bundles := []plugin.Bundle{
		plugin.WorkloadOf("sast", plugin.BundleOf(plugin.Define("sast-worker", func(plugin.BuildContext) (*validationValue, error) {
			return &validationValue{}, nil
		})), plugin.WithReplicas(3)),
		plugin.WorkloadOf("sca", plugin.BundleOf(plugin.Define("sca-worker", func(plugin.BuildContext) (*validationValue, error) {
			return &validationValue{}, nil
		}))),
	}

	env, err := config.NewEnvironment(map[string]any{"workloads": map[string]any{
		"sast": map[string]any{"enabled": true, "max_goroutines": 64},
		"sca":  map[string]any{"enabled": false},
	}}, "XBC_TEST_UNSET_")
	require.NoError(t, err)

	sections, err := ReadWorkloadSections(bundles, env)
	require.NoError(t, err)
	require.Len(t, sections, 2, "every declared workload is reported, sorted by key")

	assert.Equal(t, plugin.WorkloadKey("sast"), sections[0].Workload.Key)
	assert.True(t, sections[0].Enabled)
	assert.Equal(t, 64, sections[0].Config.MaxGoroutines)

	assert.Equal(t, plugin.WorkloadKey("sca"), sections[1].Workload.Key)
	assert.False(t, sections[1].Enabled, "enabled: false is the deployment's hard veto")
	assert.Zero(t, sections[1].Config.MaxGoroutines)

	// A composition declaring no workload reports none, which is what keeps
	// the workloads root out of that process's configuration universe.
	empty, err := ReadWorkloadSections(nil, env)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestWorkloadSectionsAreDeclaredOnlyWhenAWorkloadIsDeclared keeps the
// workloads root out of a composition that has none. An always-present
// namespace would claim a prefix nothing beneath it answers for, so a
// "workloads.sast" typo would be reported against a sibling list containing
// only itself.
func TestWorkloadSectionsAreDeclaredOnlyWhenAWorkloadIsDeclared(t *testing.T) {
	t.Parallel()
	plain := plugin.Define("plain", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	carried := plugin.Define("carried", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	workloadBundle := plugin.WorkloadOf("sast", plugin.BundleOf(carried), plugin.WithReplicas(3))

	sections, err := ConfigSections([]plugin.Bundle{plugin.BundleOf(plain), workloadBundle})
	require.NoError(t, err)
	byPath := make(map[string]config.Section, len(sections))
	for _, section := range sections {
		byPath[section.Path] = section
	}

	require.Contains(t, byPath, workloadsRoot)
	assert.Equal(t, config.SectionNamespace, byPath[workloadsRoot].Kind,
		"a nested section's parent must be a namespace, or NewUniverse rejects the whole set")

	require.Contains(t, byPath, "workloads.sast")
	assert.Equal(t, config.SectionTyped, byPath["workloads.sast"].Kind)
	assert.True(t, byPath["workloads.sast"].Toggle, "the enabled flag is what a deployment vetoes with")
	assert.Equal(t, reflect.TypeOf(WorkloadConfig{}), byPath["workloads.sast"].Schema,
		"a real schema is what makes max_goroutines validated, defaulted and env-addressable")

	// The declared set must be usable as-is: this is the universe the runtime
	// hands to config.Load.
	_, err = config.NewUniverse(sections...)
	require.NoError(t, err)

	// One namespace section no matter how many Bundles declare a workload.
	namespaceCount := 0
	for _, section := range sections {
		if section.Path == workloadsRoot {
			namespaceCount++
		}
	}
	assert.Equal(t, 1, namespaceCount, "the workloads root is emitted once")

	without, err := ConfigSections([]plugin.Bundle{plugin.BundleOf(plain)})
	require.NoError(t, err)
	for _, section := range without {
		assert.NotEqual(t, workloadsRoot, section.Path,
			"a composition declaring no workload declares no workloads root")
	}
}

// TestWorkloadConfigurationRejectsAMisspelledOrNegativeKey pins the two
// mistakes the section's own schema is there to catch, so neither reaches a
// running process as a silently ignored key.
func TestWorkloadConfigurationRejectsAMisspelledOrNegativeKey(t *testing.T) {
	t.Parallel()
	bundles := []plugin.Bundle{plugin.WorkloadOf("sast", plugin.BundleOf(plugin.Define("sast-worker", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})), plugin.WithReplicas(3))}

	misspelled, err := config.NewEnvironment(map[string]any{"workloads": map[string]any{
		"sast": map[string]any{"max_goroutine": 64},
	}}, "XBC_TEST_UNSET_")
	require.NoError(t, err)
	_, err = ReadWorkloadSections(bundles, misspelled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_goroutine")

	negative, err := config.NewEnvironment(map[string]any{"workloads": map[string]any{
		"sast": map[string]any{"max_goroutines": -1},
	}}, "XBC_TEST_UNSET_")
	require.NoError(t, err)
	_, err = ReadWorkloadSections(bundles, negative)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workloads.sast.max_goroutines")
}

// TestConstructPublishesTheProducersWorkloadOnEveryEntry is the end-to-end half
// of Entry[T].Workload: the value a consumer reads has to name the workload
// that actually produced it, because that is what a per-workload resource
// budget charges against.
func TestConstructPublishesTheProducersWorkloadOnEveryEntry(t *testing.T) {
	t.Parallel()
	producer := plugin.Define("producer", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{number: 7}, nil
	}, plugin.Options[*validationValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *validationValue) coreContract { return value })),
	})
	unowned := plugin.Define("unowned", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{number: 9}, nil
	}, plugin.Options[*validationValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *validationValue) coreContract { return value })),
	})
	many := plugin.Collect[coreContract]()
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*int, error) {
		entries := many.Get(context)
		if len(entries) != 2 {
			return nil, fmt.Errorf("expected 2 producers, got %d", len(entries))
		}
		byName := map[plugin.Key]plugin.WorkloadKey{}
		for _, entry := range entries {
			byName[entry.Identity.Plugin] = entry.Workload
		}
		if byName["producer"] != "sast" || byName["unowned"] != "" {
			return nil, fmt.Errorf("workload attribution is wrong: %v", byName)
		}
		return new(int), nil
	}, plugin.Options[*int]{Inputs: plugin.Inputs(many)})

	plan, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{
			plugin.WorkloadOf("sast", plugin.BundleOf(producer), plugin.WithReplicas(1)),
			plugin.BundleOf(unowned, consumer),
		},
		Env:       testEnvironment(t, nil),
		Placement: plugin.Placement{Source: "static", Hosted: []plugin.WorkloadKey{"sast"}},
	})
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err, "the consumer asserts the attribution its factory observed")
}
