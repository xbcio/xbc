package assembly

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

type validationValue struct{ number int }

func (value *validationValue) Number() int { return value.number }

// rawDefinition builds a Definition straight from an erased descriptor so the
// freeze-time validation branches that the typed public constructors make
// unreachable can still be exercised.
func rawDefinition(descriptor pluginmodel.DefinitionDescriptor) plugin.Definition {
	if descriptor.Primary == nil {
		descriptor.Primary = reflect.TypeOf(&validationValue{})
	}
	if descriptor.Plan == nil {
		descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) {
			return pluginmodel.InstancePlan{
				Factory: func(pluginmodel.BuildContext) (any, error) { return &validationValue{}, nil },
			}, nil
		}
	}
	return plugin.Definition(pluginmodel.NewDefinition(descriptor))
}

func planFor(t *testing.T, values map[string]any, definitions ...plugin.Definition) (*Plan, error) {
	t.Helper()
	return BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(definitions...)},
		Env:     testEnvironment(t, values),
	})
}

func TestFreezeRejectsZeroDefinitionsAndMalformedDeclarations(t *testing.T) {
	t.Parallel()
	// Definition is an opaque handle produced by plugin.Define, so the only
	// way an application can present an unbuilt one is by declaring the zero
	// value -- never by writing a struct literal.
	var unbuilt plugin.Definition
	_, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(unbuilt)},
		Env:     testEnvironment(t, nil),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains a zero Definition")

	contract := reflect.TypeOf((*interface{ Number() int })(nil)).Elem()
	primary := reflect.TypeOf(&validationValue{})
	for name, testCase := range map[string]struct {
		descriptor pluginmodel.DefinitionDescriptor
		message    string
	}{
		"invalid key": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "Web", Origin: "here.go:1"},
			message:    "invalid Definition declared at here.go:1",
		},
		"invalid cardinality": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Cardinality: pluginmodel.Cardinality(7)},
			message:    `plugin "web" has invalid cardinality 7`,
		},
		"invalid config path": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", ConfigPath: "Web.Server"},
			message:    `plugin "web" has invalid config path "Web.Server"`,
		},
		"invalid activation path": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Activation: pluginmodel.Activation{
				Kind: pluginmodel.ActivationConfigured, Path: "plugins.Web",
			}},
			message: `plugin "web" has invalid activation path "plugins.Web"`,
		},
		"invalid activation kind": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Activation: pluginmodel.Activation{
				Kind: pluginmodel.ActivationKind(9),
			}},
			message: `plugin "web" has invalid activation kind 9`,
		},
		"interface primary": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Primary: contract},
			message:    `plugin "web" primary result type must be concrete`,
		},
		"non-interface contract": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Contracts: []pluginmodel.Contract{
				{Type: reflect.TypeOf(0), Origin: "contract.go:2"},
			}},
			message: `plugin "web" additional contract must be an interface, got int at contract.go:2`,
		},
		"unassignable contract": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Primary: reflect.TypeOf(0), Contracts: []pluginmodel.Contract{
				{Type: contract, Origin: "contract.go:3"},
			}},
			message: `plugin "web" primary type int is not assignable to contract`,
		},
		"duplicate contract": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Primary: primary, Contracts: []pluginmodel.Contract{
				{Type: contract, Origin: "first.go:1"},
				{Type: contract, Origin: "second.go:1"},
			}},
			message: `declares contract`,
		},
		"nil config type": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Config: &pluginmodel.ConfigDescriptor{}},
			message:    `plugin "web" has an invalid nil configuration type`,
		},
		"nil defaults": {
			descriptor: pluginmodel.DefinitionDescriptor{Key: "web", Config: &pluginmodel.ConfigDescriptor{
				Type: reflect.TypeOf(struct{}{}),
			}},
			message: `plugin "web" ConfigSpec.Defaults cannot be nil`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := planFor(t, nil, rawDefinition(testCase.descriptor))
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
		})
	}
}

func TestFreezeRejectsADefinitionWithoutAPlanner(t *testing.T) {
	t.Parallel()
	descriptor := pluginmodel.DefinitionDescriptor{Key: "web", Primary: reflect.TypeOf(&validationValue{})}
	_, err := planFor(t, nil, plugin.Definition(pluginmodel.NewDefinition(descriptor)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `plugin "web" has no planner/factory`)
}

type dualLifecycleValue struct{}

func (dualLifecycleValue) Init(*plugin.Context) error        { return nil }
func (dualLifecycleValue) Migrate(*plugin.Context) error     { return nil }
func (dualLifecycleValue) Start(*plugin.Context) error       { return nil }
func (dualLifecycleValue) OpenTraffic(*plugin.Context) error { return nil }
func (dualLifecycleValue) Stop(ctx context.Context) error    { return nil }
func (dualLifecycleValue) Number() int                       { return 0 }

func TestFreezeRejectsAStageDeclaredByBothTheTypeAndAnAdapter(t *testing.T) {
	t.Parallel()
	for stage, lifecycle := range map[string]plugin.Lifecycle[dualLifecycleValue]{
		"Init":        {Init: func(dualLifecycleValue, *plugin.Context) error { return nil }},
		"Migrate":     {Migrate: func(dualLifecycleValue, *plugin.Context) error { return nil }},
		"Start":       {Start: func(dualLifecycleValue, *plugin.Context) error { return nil }},
		"OpenTraffic": {OpenTraffic: func(dualLifecycleValue, *plugin.Context) error { return nil }},
		"Stop":        {Stop: func(dualLifecycleValue, context.Context) error { return nil }},
	} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			definition := plugin.Define("dual", func(plugin.BuildContext) (dualLifecycleValue, error) {
				return dualLifecycleValue{}, nil
			}, plugin.Options[dualLifecycleValue]{Lifecycle: lifecycle})
			_, err := planFor(t, nil, definition)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "implements "+stage+" and also declares an adapter for that stage")
		})
	}
}

func TestActivationAndEnabledFlagsDecideExpansion(t *testing.T) {
	t.Parallel()
	always := plugin.Define("always", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	configured := plugin.Define("configured", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Activation: plugin.WhenConfigured("plugins.configured")})

	plan, err := planFor(t, nil, always, configured)
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{{Plugin: "always", Instance: plugin.DefaultInstance}}, plan.Order())
	assert.Equal(t, []plugin.Key{"configured"}, plan.Disabled())
	assert.Equal(t, 2, plan.DefinitionCount())

	plan, err = planFor(t, map[string]any{"plugins": map[string]any{
		"configured": map[string]any{},
	}}, always, configured)
	require.NoError(t, err)
	assert.Len(t, plan.Order(), 2, "an existing activation path enables the plugin")
	assert.Empty(t, plan.Disabled())

	plan, err = planFor(t, map[string]any{"plugins": map[string]any{
		"always": map[string]any{"enabled": false},
	}}, always)
	require.NoError(t, err)
	assert.Empty(t, plan.Order())
	assert.Equal(t, []plugin.Key{"always"}, plan.Disabled())

	_, err = planFor(t, map[string]any{"plugins": map[string]any{
		"always": map[string]any{"enabled": "yes"},
	}}, always)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.always.enabled must be boolean, got yes")
}

func TestMultiInstanceExpansionValidatesNamesAndSectionShape(t *testing.T) {
	t.Parallel()
	definition := plugin.DefineConfigured("gorm", plugin.ConfigSpec[struct {
		DSN string `yaml:"dsn"`
	}]{
		Defaults: func() struct {
			DSN string `yaml:"dsn"`
		} {
			return struct {
				DSN string `yaml:"dsn"`
			}{}
		},
	}, func(_ plugin.BuildContext, config struct {
		DSN string `yaml:"dsn"`
	}) (*validationValue, error) {
		return &validationValue{number: len(config.DSN)}, nil
	}, plugin.Options[*validationValue]{Instances: plugin.MultipleInstances})

	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{{Plugin: "gorm", Instance: plugin.DefaultInstance}}, plan.Order(),
		"an unconfigured multi-instance plugin still expands to its default instance")

	plan, err = planFor(t, map[string]any{"plugins": map[string]any{"gorm": map[string]any{
		"readonly": map[string]any{"dsn": "ro"},
		"primary":  map[string]any{"dsn": "rw"},
		"archived": map[string]any{"enabled": false},
	}}}, definition)
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{
		{Plugin: "gorm", Instance: "primary"},
		{Plugin: "gorm", Instance: "readonly"},
	}, plan.Order(), "instances expand in sorted order and a disabled instance is skipped")

	plan, err = planFor(t, map[string]any{"plugins": map[string]any{"gorm": map[string]any{
		"enabled":  false,
		"readonly": map[string]any{"dsn": "ro"},
	}}}, definition)
	require.NoError(t, err)
	assert.Empty(t, plan.Order(), "the section-level flag disables every instance")
	assert.Equal(t, []plugin.Key{"gorm"}, plan.Disabled())

	for name, testCase := range map[string]struct {
		section map[string]any
		message string
	}{
		"invalid instance name": {
			section: map[string]any{"Read Only": map[string]any{}},
			message: `multi-instance plugin gorm has invalid instance "Read Only"`,
		},
		"scalar that is not enabled": {
			section: map[string]any{"dsn": "shared"},
			message: `multi-instance plugin gorm configuration "dsn" is not an instance map`,
		},
		"non-boolean section flag": {
			section: map[string]any{"enabled": "yes"},
			message: "plugins.gorm.enabled must be boolean, got yes",
		},
		"non-boolean instance flag": {
			section: map[string]any{"readonly": map[string]any{"enabled": "yes"}},
			message: "plugins.gorm.readonly.enabled must be boolean, got yes",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := planFor(t, map[string]any{"plugins": map[string]any{"gorm": testCase.section}}, definition)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
		})
	}
}

func TestDeclaredSectionThatIsOffIsADisablementNotAnOrphan(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("known", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})

	plan, err := planFor(t, map[string]any{"plugins": map[string]any{
		"known": map[string]any{"enabled": false},
	}}, definition)
	require.NoError(t, err, "a disabled but declared plugin is not an orphan")
	assert.Equal(t, []plugin.Key{"known"}, plan.Disabled())
}

func TestConfigurationFailuresNameTheOwningInstance(t *testing.T) {
	t.Parallel()
	type cfg struct {
		Value string `yaml:"value" validate:"required"`
	}
	spec := func(defaults func() cfg, prepare func(cfg) (cfg, error)) plugin.Definition {
		return plugin.DefineConfigured("cfg", plugin.ConfigSpec[cfg]{Defaults: defaults, Prepare: prepare},
			func(plugin.BuildContext, cfg) (*validationValue, error) { return &validationValue{}, nil })
	}

	_, err := planFor(t, nil, spec(func() cfg { panic("boom") }, nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin cfg ConfigSpec.Defaults panic: boom")

	_, err = planFor(t, nil, spec(func() cfg { return cfg{} }, nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin cfg configuration validation failed")

	_, err = planFor(t, map[string]any{"plugins": map[string]any{"cfg": map[string]any{"unknown": 1, "value": "v"}}},
		spec(func() cfg { return cfg{} }, nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin cfg failed to bind configuration plugins.cfg")

	_, err = planFor(t, nil, spec(func() cfg { return cfg{Value: "v"} }, func(cfg) (cfg, error) { panic("prepare boom") }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin cfg configuration Prepare panic: prepare boom")

	_, err = planFor(t, nil, spec(func() cfg { return cfg{Value: "v"} }, func(cfg) (cfg, error) {
		return cfg{}, errors.New("refused")
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin cfg configuration Prepare failed: refused")
}

func TestDefaultsReturningTheWrongShapeIsRejectedBeforeBinding(t *testing.T) {
	t.Parallel()
	descriptor := pluginmodel.DefinitionDescriptor{
		Key:     "wrong",
		Primary: reflect.TypeOf(&validationValue{}),
		Config: &pluginmodel.ConfigDescriptor{
			Type:     reflect.TypeOf(struct{ Value string }{}),
			Defaults: func() any { return nil },
		},
	}
	_, err := planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin wrong ConfigSpec.Defaults returned nil")

	descriptor.Config.Defaults = func() any { return 42 }
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ConfigSpec.Defaults returned int, want struct")

	descriptor.Config.Defaults = func() any { return struct{ Value string }{} }
	descriptor.Config.Prepare = func(any) (any, error) { return 42, nil }
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configuration Prepare returned int, want struct")
}

func TestPlannerFailuresAndInvalidTokensAreRejectedDuringPlanning(t *testing.T) {
	t.Parallel()
	descriptor := pluginmodel.DefinitionDescriptor{Key: "planner", Primary: reflect.TypeOf(&validationValue{})}

	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) { panic("planner boom") }
	_, err := planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin planner planner panic: planner boom")

	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) {
		return pluginmodel.InstancePlan{}, errors.New("refused")
	}
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin planner planning failed: refused")

	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) { return pluginmodel.InstancePlan{}, nil }
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin planner planner returned nil factory")

	factory := func(pluginmodel.BuildContext) (any, error) { return &validationValue{}, nil }
	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) {
		return pluginmodel.InstancePlan{Factory: factory, Inputs: []pluginmodel.InputToken{{}}}, nil
	}
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin planner plan contains invalid input token")

	token := pluginmodel.NewInputToken(pluginmodel.QueryMany, reflect.TypeOf(0), "", "", "origin.go:1")
	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) {
		return pluginmodel.InstancePlan{Factory: factory, Inputs: []pluginmodel.InputToken{token, token}}, nil
	}
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares input token")
	assert.Contains(t, err.Error(), "twice (origin.go:1 and origin.go:1)")

	descriptor.Plan = func(any) (pluginmodel.InstancePlan, error) {
		return pluginmodel.InstancePlan{Factory: factory, Inputs: []pluginmodel.InputToken{{
			ID: 1, Type: reflect.TypeOf(0), Kind: pluginmodel.QueryKind(99),
		}}}, nil
	}
	_, err = planFor(t, nil, rawDefinition(descriptor))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown query kind 99")
}
