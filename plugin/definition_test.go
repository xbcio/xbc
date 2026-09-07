package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

type definitionContract interface{ Value() int }
type definitionValue struct{ value int }

func (value *definitionValue) Value() int { return value.value }

func descriptorOf(t *testing.T, definition Definition) pluginmodel.DefinitionDescriptor {
	t.Helper()
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
	require.True(t, ok)
	return descriptor
}

func TestDefinitionIsCanonicalOpaqueHandle(t *testing.T) {
	canonical := Define("canonical", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{value: 1}, nil
	})
	definition := func() Definition { return canonical }
	assert.Equal(t, definition(), definition())

	other := Define("canonical", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{value: 1}, nil
	})
	assert.NotEqual(t, canonical, other, "same metadata does not make two handles identical")
}

func TestDefineFreezesStaticMetadata(t *testing.T) {
	definition := Define("fixture", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{value: 2}, nil
	}, Options[*definitionValue]{
		Instances:  MultipleInstances,
		Activation: WhenConfigured("plugins.fixture"),
		ConfigPath: "services.fixture",
		Exports: Contracts(
			ExportAs(func(value *definitionValue) definitionContract { return value }),
		),
		Lifecycle: Lifecycle[*definitionValue]{
			Stop: func(*definitionValue, context.Context) error { return nil },
		},
	})
	descriptor := descriptorOf(t, definition)
	assert.Equal(t, pluginmodel.Key("fixture"), descriptor.Key)
	assert.Equal(t, pluginmodel.MultipleInstances, descriptor.Cardinality)
	assert.Equal(t, pluginmodel.ActivationConfigured, descriptor.Activation.Kind)
	assert.Equal(t, "plugins.fixture", descriptor.Activation.Path)
	assert.Equal(t, "services.fixture", descriptor.ConfigPath)
	require.Len(t, descriptor.Contracts, 1)
	assert.Equal(t, typeOf[definitionContract](), descriptor.Contracts[0].Type)
	assert.NotNil(t, descriptor.Lifecycle.Stop)
}

func TestConfiguredDefinitionErasesFreshDefaultsAndPrepare(t *testing.T) {
	type config struct{ Number int }
	calls := 0
	definition := DefineConfigured("configured", ConfigSpec[config]{
		Defaults: func() config {
			calls++
			return config{Number: calls}
		},
		Prepare: func(value config) (config, error) {
			value.Number *= 2
			return value, nil
		},
	}, func(BuildContext, config) (*definitionValue, error) {
		return &definitionValue{}, nil
	})
	descriptor := descriptorOf(t, definition)
	require.NotNil(t, descriptor.Config)
	first := descriptor.Config.Defaults().(config)
	second := descriptor.Config.Defaults().(config)
	assert.Equal(t, config{Number: 1}, first)
	assert.Equal(t, config{Number: 2}, second)
	prepared, err := descriptor.Config.Prepare(first)
	require.NoError(t, err)
	assert.Equal(t, config{Number: 2}, prepared)
}

func TestDefinitionHelpersRejectAmbiguousOptionShapes(t *testing.T) {
	factory := func(BuildContext) (*definitionValue, error) { return &definitionValue{}, nil }
	assert.PanicsWithValue(t, "xbc: plugin definition accepts at most one Options value", func() {
		Define("too-many", factory, Options[*definitionValue]{}, Options[*definitionValue]{})
	})
	assert.PanicsWithValue(t, "xbc: plugin.ExportAs witness cannot be nil", func() {
		ExportAs[definitionContract, *definitionValue](nil)
	})
}
