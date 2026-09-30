package assembly

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

type activationProbeConfig struct {
	Value string `yaml:"value" default:"probe"`
}

// TestActivationDoesNotDependOnDefinitionOrder pins that whether a Definition
// activates is a function of the user's configuration alone, never of which
// Definitions happened to bind their sections first.
//
// Binding writes a section's fully-bound leaves back into the environment,
// default tags included, so that a section built entirely from defaults is
// still visible to Get/Exists/Sub. That write makes the path look configured to
// every later reader. Because freezeBundles sorts Definitions by key, "later"
// would otherwise mean "sorts after": the same composition would activate
// differently under different plugin keys.
//
// The fixture is the shape that makes it observable. "activating" is enabled by
// another section and owns plugins.activating, which it binds. "watching" is
// enabled by plugins.activating, which the user never wrote -- so it must stay
// off no matter which of the two the key order visits first.
func TestActivationDoesNotDependOnDefinitionOrder(t *testing.T) {
	t.Parallel()
	activating := plugin.DefineConfigured("activating", plugin.ConfigSpec[activationProbeConfig]{
		Defaults: func() activationProbeConfig { return activationProbeConfig{} },
	}, func(plugin.BuildContext, activationProbeConfig) (*coreValue, error) {
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{
		Activation: plugin.WhenConfigured("plugins.other"),
	})
	watching := plugin.Define("watching", func(plugin.BuildContext) (*coreValue, error) {
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{
		Activation: plugin.WhenConfigured("plugins.activating"),
	})

	plan, err := planFor(t, map[string]any{
		"plugins": map[string]any{"other": map[string]any{"value": "set"}},
	}, activating, watching)
	require.NoError(t, err)

	assert.Contains(t, plan.Order(), plugin.Identity{Plugin: "activating", Instance: plugin.DefaultInstance},
		"the section the user actually wrote enables its Definition")
	assert.NotContains(t, plan.Order(), plugin.Identity{Plugin: "watching", Instance: plugin.DefaultInstance},
		"plugins.activating was never configured by the user")
	assert.Contains(t, plan.Disabled(), plugin.Key("watching"))

	// The reason has to name the section the user would have to write, not the
	// section that merely got bound along the way.
	reasons := map[plugin.Key]string{}
	for _, entry := range plan.DisabledDetail() {
		reasons[entry.Key] = entry.Reason
	}
	assert.Contains(t, reasons[plugin.Key("watching")], "plugins.activating is not configured")
}
