package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
)

// runtimeTestStarter is a baseline the runtime tests own. The runtime never
// names a real starter -- the one this repository ships is a package of the
// Web module, which a core package may not import -- so the option's
// behaviour is observed through a value whose contents this package chooses.
type runtimeTestStarter struct {
	bundles  []plugin.Bundle
	defaults config.Defaults
}

func (s runtimeTestStarter) Bundles() []plugin.Bundle  { return s.bundles }
func (s runtimeTestStarter) Defaults() config.Defaults { return s.defaults }

// dormantDefinition is a plugin that exists only when its section does, which
// is the shape every capability a starter switches on has.
func dormantDefinition(key plugin.Key) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins." + key.String())})
}

// TestWithStarterSuppliesBothHalvesOfABaseline pins the property that makes a
// starter worth having as one value: on a process whose configuration file
// says nothing about the capability, selecting the starter both composes the
// Definition and supplies the section that activates it.
//
// The configuration is the ordinary quiet fixture, so the activation can only
// have come from the defaults layer. The process catalog assertion is the
// other half of the same value: selecting a starter is an explicit
// composition, so the optional process-global collector plays no part in it.
func TestWithStarterSuppliesBothHalvesOfABaseline(t *testing.T) {
	starter := runtimeTestStarter{
		bundles: []plugin.Bundle{plugin.BundleOf(dormantDefinition("starter-activated"))},
		defaults: config.Defaults{
			Label:  "test baseline",
			Values: map[string]any{"plugins.starter-activated.enabled": true},
		},
	}

	app, err := New(WithStarter(starter))
	require.NoError(t, err)
	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "planned 1, enabled instances 1, disabled 0",
		"a capability the defaults layer names must be constructed without a configuration file saying so")
	assert.Contains(t, out, "defaults (test baseline)",
		"doctor names the starter as where the section came from")
	assert.NotContains(t, runtimeTestPlanKeys(app.plan), processCatalogSentinel,
		"an explicitly composed App composes its starter, not the frozen process catalog")
}

// TestWithStarterDefaultsLoseToEveryHigherLayer pins that the layer is a
// default and not an override. A starter that could not be disagreed with
// would make a capability unavoidable in every process that selected it, and
// the escape hatch has to work from the environment too -- that is the only
// channel a container deployment has.
func TestWithStarterDefaultsLoseToEveryHigherLayer(t *testing.T) {
	baseline := runtimeTestStarter{
		bundles: []plugin.Bundle{plugin.BundleOf(dormantDefinition("starter-activated"))},
		defaults: config.Defaults{
			Label:  "test baseline",
			Values: map[string]any{"plugins.starter-activated.enabled": true},
		},
	}

	t.Run("a configuration file wins", func(t *testing.T) {
		app, err := New(WithStarter(baseline))
		require.NoError(t, err)

		out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
			"plugins:\n  starter-activated:\n    enabled: false\n")...)

		assert.Contains(t, out, "planned 1, enabled instances 0, disabled 1",
			"a file that turns the capability off must win over the baseline")
		assert.Contains(t, out, "plugins.starter-activated.enabled is false",
			"the reason names the file's own value, which is what proves the higher layer was read")
		assert.Contains(t, out, "defaults (test baseline)",
			"the baseline still contributed the section the file disagrees with")
	})

	t.Run("an environment variable wins", func(t *testing.T) {
		t.Setenv("XBC_PLUGINS_STARTER_ACTIVATED_ENABLED", "false")

		app, err := New(WithStarter(baseline))
		require.NoError(t, err)

		out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

		assert.Contains(t, out, "planned 1, enabled instances 0, disabled 1",
			"the environment is the half a container deployment has, and it must outrank the baseline")
		assert.Contains(t, out, "plugins.starter-activated.enabled is false",
			"the environment layer's value is the one the plugin was disabled by")
	})
}

// TestWithStarterDefaultsCannotNameAPathNobodyOwns keeps the loader's
// fail-closed ownership rule over the new layer, and pins that a starter's
// mistake is reported to the process that made it. A baseline assembled from
// Go values rather than typed by a user is exactly where a wrong path could
// otherwise reach a deployment as a section that silently does nothing.
func TestWithStarterDefaultsCannotNameAPathNobodyOwns(t *testing.T) {
	starter := runtimeTestStarter{
		bundles: []plugin.Bundle{plugin.BundleOf(dormantDefinition("starter-activated"))},
		defaults: config.Defaults{
			Label:  "test baseline",
			Values: map[string]any{"plugins.starter-activted.enabled": true},
		},
	}

	app, err := New(WithStarter(starter))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), append([]string{"doctor"}, runtimeTestConfig(t, time.Second)...))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.starter-activted",
		"the failure names the path the baseline got wrong")
	assert.Contains(t, err.Error(), "plugins.starter-activated",
		"and names the section declared beside it, which is what the reader needs to fix it")
}

func TestWithStarterRejectsANilStarter(t *testing.T) {
	_, err := New(WithStarter(nil))
	assert.EqualError(t, err,
		"xbc: WithStarter requires a non-nil Starter; omit the option to compose Bundles explicitly")
}
