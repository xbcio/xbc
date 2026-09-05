package assembly

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
)

// TestDisabledDetailExplainsEveryWayAPluginCanBeOff pins the disable-reason
// surface. Reporting only "off" is what makes "I selected the plugin and
// nothing happened" indistinguishable from "it failed silently", so each of
// the three ways a Definition can end up disabled must produce a distinct,
// path-naming reason.
func TestDisabledDetailExplainsEveryWayAPluginCanBeOff(t *testing.T) {
	t.Parallel()
	unconfigured := plugin.Define("unconfigured", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Activation: plugin.WhenConfigured("plugins.unconfigured")})
	single := plugin.Define("single", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	multi := plugin.Define("multi", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Instances: plugin.MultipleInstances})

	plan, err := planFor(t, map[string]any{"plugins": map[string]any{
		"single": map[string]any{"enabled": false},
		"multi":  map[string]any{"only": map[string]any{"enabled": false}},
	}}, unconfigured, single, multi)
	require.NoError(t, err)

	reasons := make(map[plugin.Key]string)
	paths := make(map[plugin.Key]string)
	for _, entry := range plan.DisabledDetail() {
		reasons[entry.Key] = entry.Reason
		paths[entry.Key] = entry.Path
	}
	assert.Equal(t, "activation path plugins.unconfigured is not configured", reasons["unconfigured"])
	assert.Equal(t, "plugins.single.enabled is false", reasons["single"])
	assert.Equal(t, "every instance configured under plugins.multi is disabled", reasons["multi"])
	assert.Equal(t, "plugins.multi", paths["multi"], "the reason is anchored to the section a reader would edit")

	assert.Equal(t, []plugin.Key{"multi", "single", "unconfigured"}, plan.Disabled(),
		"the key-only projection keeps its existing contract")
}

// TestInstanceConfigPathNamesTheSectionEachInstanceBinds is what lets a
// diagnostic point a reader at the exact block to edit, including for the two
// shapes where the section is not simply "plugins.<key>".
func TestInstanceConfigPathNamesTheSectionEachInstanceBinds(t *testing.T) {
	t.Parallel()
	single := plugin.Define("single", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	multi := plugin.Define("multi", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Instances: plugin.MultipleInstances})
	custom := plugin.Define("custom", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{ConfigPath: "web"})

	plan, err := planFor(t, map[string]any{"plugins": map[string]any{
		"multi": map[string]any{"primary": map[string]any{}},
	}}, single, multi, custom)
	require.NoError(t, err)

	assert.Equal(t, "plugins.single",
		plan.InstanceConfigPath(plugin.Identity{Plugin: "single", Instance: plugin.DefaultInstance}))
	assert.Equal(t, "plugins.multi.primary",
		plan.InstanceConfigPath(plugin.Identity{Plugin: "multi", Instance: "primary"}))
	assert.Equal(t, "web",
		plan.InstanceConfigPath(plugin.Identity{Plugin: "custom", Instance: plugin.DefaultInstance}))
	assert.Empty(t, plan.InstanceConfigPath(plugin.Identity{Plugin: "absent"}),
		"an identity this plan never resolved has no section")
}

// TestCustomConfigPathUnderThePluginsRootIsNotAnOrphan closes the gap where a
// Definition that renames its section but stays under plugins.* was excluded
// from the orphan scan, so its own configuration was reported as belonging to
// no plugin.
func TestCustomConfigPathUnderThePluginsRootIsNotAnOrphan(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("renamed", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{ConfigPath: "plugins.actual-section"})

	plan, err := planFor(t, map[string]any{"plugins": map[string]any{
		"actual-section": map[string]any{"enabled": true},
	}}, definition)
	require.NoError(t, err, "the section the Definition actually declares is not an orphan")
	assert.Equal(t, "plugins.actual-section",
		plan.InstanceConfigPath(plugin.Identity{Plugin: "renamed", Instance: plugin.DefaultInstance}))

	_, err = planFor(t, map[string]any{"plugins": map[string]any{
		"renamed": map[string]any{"enabled": true},
	}}, definition)
	require.Error(t, err, "the conventional name is not a fallback once ConfigPath is set")
	assert.Contains(t, err.Error(), "plugins.renamed has configuration but no corresponding plugin")
}

// TestConfigSectionsDeclareOwnershipForEverySelectedDefinition is the bridge
// between the plugin graph and the configuration ownership model: whatever a
// composition root selected must end up owning its own section, or a legitimate
// configuration block would be rejected as unowned.
func TestConfigSectionsDeclareOwnershipForEverySelectedDefinition(t *testing.T) {
	t.Parallel()
	type cfg struct {
		DSN string `yaml:"dsn"`
	}
	conventional := plugin.Define("conventional", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	custom := plugin.DefineConfigured("custom", plugin.ConfigSpec[cfg]{
		Defaults: func() cfg { return cfg{} },
	}, func(plugin.BuildContext, cfg) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{ConfigPath: "web", Instances: plugin.MultipleInstances})

	sections, err := ConfigSections([]plugin.Bundle{plugin.BundleOf(conventional, custom)})
	require.NoError(t, err)

	byPath := make(map[string]config.Section, len(sections))
	for _, section := range sections {
		byPath[section.Path] = section
	}

	require.Contains(t, byPath, pluginsRoot)
	assert.Equal(t, config.SectionNamespace, byPath[pluginsRoot].Kind,
		"the plugins root claims the prefix without accepting keys of its own")

	require.Contains(t, byPath, "plugins.conventional")
	assert.Equal(t, config.SectionTyped, byPath["plugins.conventional"].Kind)
	assert.True(t, byPath["plugins.conventional"].Toggle, "every plugin section carries the enabled flag")
	assert.Nil(t, byPath["plugins.conventional"].Schema, "a plugin without a config struct declares no schema")

	require.Contains(t, byPath, "web", "a custom ConfigPath claims its own top-level section")
	assert.Equal(t, config.SectionInstanced, byPath["web"].Kind)
	assert.Equal(t, reflect.TypeOf(cfg{}), byPath["web"].Schema,
		"the schema is what lets an environment variable resolve to a leaf of this section")

	// The declared set must be usable as-is: a Universe built from it is what
	// the runtime hands to config.Load.
	_, err = config.NewUniverse(sections...)
	require.NoError(t, err)
}
