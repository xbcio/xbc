package httpclient

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func httpclientEnvironment(t *testing.T, instances map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{"http-client": instances},
	}, "XBC_HTTPCLIENT_PLUGIN_TEST_")
	require.NoError(t, err)
	return environment
}

func TestDefinitionAndBundleAreStable(t *testing.T) {
	var zero plugin.Definition
	assert.NotEqual(t, zero, Definition())
	assert.Equal(t, Definition(), Definition())
	first, second := Bundle(), Bundle()
	assert.NotEqual(t, plugin.Bundle{}, first)
	assert.True(t, reflect.DeepEqual(first, second))
}

func TestPlanExpandsAndConstructsEachNamedInstance(t *testing.T) {
	environment := httpclientEnvironment(t, map[string]any{
		"gateway": map[string]any{"base_url": "https://gateway.example"},
		"ledger":  map[string]any{"base_url": "https://ledger.example"},
	})
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []plugin.Identity{
		{Plugin: Key, Instance: "gateway"},
		{Plugin: Key, Instance: "ledger"},
	}, plan.Order(), "both named instances are planned")

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	require.NoError(t, err)
	for name, endpoint := range map[string]string{
		"gateway": "https://gateway.example",
		"ledger":  "https://ledger.example",
	} {
		instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: name})
		require.True(t, found, "instance %q not constructed", name)
		client, ok := instance.Primary().(*Client)
		require.True(t, ok, "instance %q primary is %T", name, instance.Primary())
		assert.Equal(t, name, client.instance)
		assert.Equal(t, endpoint, client.config.BaseURL)
		require.NotNil(t, client.Client())
	}

	// Each instance contributes its own configuration path, so a missing one
	// fails the plan instead of silently sharing a sibling's settings.
	missing := httpclientEnvironment(t, map[string]any{})
	_, err = assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle()}, Env: missing})
	require.NoError(t, err, "no configured instance means the WhenConfigured activation stays off")
}

// gatewayHandle is the declared binding a consumer would hold: an exact typed
// input for one named instance of this plugin, read from the graph by key.
var gatewayHandle = plugin.RefToInstance[*Client](Key, "gateway")

type consumerProbe struct {
	client *Client
}

var consumerProbeDefinition = plugin.Define(
	"httpclient-consumer-probe",
	func(ctx plugin.BuildContext) (*consumerProbe, error) {
		return &consumerProbe{client: gatewayHandle.Get(ctx).Value}, nil
	},
	plugin.Options[*consumerProbe]{
		Inputs: plugin.Inputs(gatewayHandle),
	},
)

func TestConsumerResolvesExactlyTheNamedInstance(t *testing.T) {
	environment := httpclientEnvironment(t, map[string]any{
		"gateway": map[string]any{"base_url": "https://gateway.example"},
		"ledger":  map[string]any{"base_url": "https://ledger.example"},
	})
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), plugin.BundleOf(consumerProbeDefinition)},
		Env:     environment,
	})
	require.NoError(t, err)
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	require.NoError(t, err)

	instance, found := constructed.Instance(plugin.Identity{Plugin: "httpclient-consumer-probe"})
	require.True(t, found)
	probe, ok := instance.Primary().(*consumerProbe)
	require.True(t, ok)
	require.NotNil(t, probe.client)
	assert.Equal(t, "gateway", probe.client.instance,
		"the consumer binds the named instance, not whichever was constructed first")
	assert.Equal(t, "https://gateway.example", probe.client.config.BaseURL)
}
