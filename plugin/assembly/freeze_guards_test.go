package assembly

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// TestFreezeRejectsTwoDefinitionsSharingOneConfigurationSection pins the
// collision guard that the configuration layer cannot state in the vocabulary
// of the mistake.
//
// config.NewUniverse does reject a duplicated section path, but it only sees
// paths: by the time it runs, the definitions that claimed them are gone, so
// its message can name neither the plugin keys nor where they were declared.
// Freezing knows both, which is why the check lives here.
func TestFreezeRejectsTwoDefinitionsSharingOneConfigurationSection(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		first           plugin.Definition
		second          plugin.Definition
		expectedSection string
	}{
		// Two plugins that both named the same path outright, which is the
		// shape a copied Options block produces.
		"explicit paths": {
			expectedSection: "plugins.shared",
			first: plugin.Define("alpha", func(plugin.BuildContext) (*coreValue, error) {
				return &coreValue{}, nil
			}, plugin.Options[*coreValue]{ConfigPath: "plugins.shared"}),
			second: plugin.Define("beta", func(plugin.BuildContext) (*coreValue, error) {
				return &coreValue{}, nil
			}, plugin.Options[*coreValue]{ConfigPath: "plugins.shared"}),
		},
		// One plugin falling back to the conventional path and another naming
		// that same path explicitly. Neither Options block is wrong on its own,
		// which is exactly why the collision is worth reporting by name.
		"conventional path claimed explicitly": {
			expectedSection: "plugins.alpha",
			first: plugin.Define("alpha", func(plugin.BuildContext) (*coreValue, error) {
				return &coreValue{}, nil
			}),
			second: plugin.Define("beta", func(plugin.BuildContext) (*coreValue, error) {
				return &coreValue{}, nil
			}, plugin.Options[*coreValue]{ConfigPath: "plugins.alpha"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := planFor(t, nil, testCase.first, testCase.second)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "configuration section "+testCase.expectedSection)
			assert.Contains(t, err.Error(), `plugins "alpha" and "beta"`,
				"both offending plugin keys are named so the reader knows who to change")
			assert.Contains(t, err.Error(), "ConfigPath",
				"the fix is spelled out: give one Definition a distinct ConfigPath")
		})
	}
}

// TestOneDefinitionMayStillOwnOneSectionAcrossItsInstances guards the natural
// false positive: a multi-instance plugin binds every instance beneath its
// single section, so a plugin declaring several instances must not be read as
// several Definitions claiming one path.
func TestOneDefinitionMayStillOwnOneSectionAcrossItsInstances(t *testing.T) {
	t.Parallel()
	definition := plugin.DefineConfigured("multi", plugin.ConfigSpec[struct{}]{
		Defaults: func() struct{} { return struct{}{} },
	}, func(plugin.BuildContext, struct{}) (*coreValue, error) {
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{Instances: plugin.MultipleInstances})

	plan, err := planFor(t, map[string]any{"plugins": map[string]any{"multi": map[string]any{
		"first":  map[string]any{},
		"second": map[string]any{},
	}}}, definition)
	require.NoError(t, err)
	require.Len(t, plan.Order(), 2, "both instances expand from the one Definition")
	assert.Equal(t, "plugins.multi.first",
		plan.InstanceConfigPath(plugin.Identity{Plugin: "multi", Instance: "first"}))
}

// TestFreezeRejectsAMalformedWorkloadKey keeps the "I misspelled the workload"
// failure from being reported as "I forgot to declare the workload". The latter
// is what ValidateWorkloads says about an unknown key, and it points the reader
// at the composition rather than at the typo.
func TestFreezeRejectsAMalformedWorkloadKey(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("member", func(plugin.BuildContext) (*coreValue, error) {
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{Workload: plugin.WorkloadKey("Sast")})

	_, err := planFor(t, nil, definition)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `plugin "member" has invalid workload "Sast"`)
	assert.Contains(t, err.Error(), "only lowercase letters, digits, underscores, and hyphens")
	assert.NotContains(t, err.Error(), "nothing in the composition declares",
		"a malformed key is a different mistake from an undeclared workload")
}

// TestInstanceAccessorsNormalizeIdentity pins that the three identity-keyed
// accessors agree with the rest of the surface about what an identity means.
//
// Every map inside a Plan is keyed by the normalized identity, and WorkloadOf
// already normalizes on read. An accessor that did not would answer "" for
// Identity{Plugin: k} -- indistinguishable from "this instance is not in the
// plan", turning a caller's omitted instance into a silent miss.
func TestInstanceAccessorsNormalizeIdentity(t *testing.T) {
	t.Parallel()
	producer := plugin.Define("producer", func(plugin.BuildContext) (*coreValue, error) {
		return &coreValue{number: 42}, nil
	}, plugin.Options[*coreValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *coreValue) coreContract { return value })),
	})
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*coreValue, error) {
		_ = plugin.RefTo[coreContract]("producer").Get(context).Value
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{
		Inputs: plugin.Inputs(plugin.RefTo[coreContract]("producer")),
	})

	plan, err := planFor(t, nil, producer, consumer)
	require.NoError(t, err)

	// The instance field is left zero, the way a caller who does not care about
	// instances writes it.
	implicit := plugin.Identity{Plugin: "consumer"}
	explicit := plugin.Identity{Plugin: "consumer", Instance: plugin.DefaultInstance}

	assert.NotEmpty(t, plan.InstanceConfigPath(explicit), "the fixture must have a path to compare")
	assert.Equal(t, plan.InstanceConfigPath(explicit), plan.InstanceConfigPath(implicit))
	assert.Equal(t, plan.InstanceSelectedAt(explicit), plan.InstanceSelectedAt(implicit))
	assert.NotEmpty(t, plan.InstanceSelectedAt(implicit))
	assert.Equal(t, plan.InstanceInputs(explicit), plan.InstanceInputs(implicit))
	assert.NotEmpty(t, plan.InstanceInputs(implicit), "the consumer declares one input to compare")

	_, known := plan.WorkloadOf(explicit)
	_, alsoKnown := plan.WorkloadOf(implicit)
	assert.Equal(t, known, alsoKnown, "every identity-keyed accessor normalizes the same way")
}

// TestCollectSelfDependencyNamesTheContractAndTheFix covers the aggregation
// mistake that a single value's self-reference cannot explain.
//
// A plugin exporting T while collecting every exporter of T is a plausible
// shape, and QueryMany deliberately keeps the consumer among its own candidates
// so the resulting edge is rejected rather than silently dropped. The reader
// still needs to be told why, because nothing in the declaration looks wrong.
func TestCollectSelfDependencyNamesTheContractAndTheFix(t *testing.T) {
	t.Parallel()
	collected := plugin.Collect[coreContract]()
	aggregator := plugin.Define("aggregator", func(context plugin.BuildContext) (*coreValue, error) {
		_ = collected.Get(context)
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{
		Inputs:  plugin.Inputs(collected),
		Exports: plugin.Contracts(plugin.ExportAs(func(value *coreValue) coreContract { return value })),
	})

	_, err := planFor(t, nil, aggregator)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coreContract", "the contract that closes the loop is named")
	assert.Contains(t, err.Error(), "self-dependency")
	assert.Contains(t, err.Error(), "split the aggregator and the exporter into two Definitions",
		"the fix is not inferable from the declaration")
}
