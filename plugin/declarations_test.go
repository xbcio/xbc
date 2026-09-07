package plugin

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestValidateNameAndInstanceNameShareTheIdentifierRuleAndNameTheirKind(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateName("gorm-readonly_2"))
	require.NoError(t, ValidateInstanceName("readonly"))

	err := ValidateName("Gorm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin key")
	assert.Contains(t, err.Error(), "contains invalid character")

	err = ValidateInstanceName("read only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instance name")

	assert.EqualError(t, ValidateName(""), "xbc: plugin key cannot be empty")
	assert.EqualError(t, ValidateInstanceName(""), "xbc: instance name cannot be empty")
}

func TestBundleOfRecordsDeclarationOriginAndCombinePreservesOccurrences(t *testing.T) {
	t.Parallel()
	first := Define("first", func(BuildContext) (*definitionValue, error) { return &definitionValue{}, nil })
	second := Define("second", func(BuildContext) (*definitionValue, error) { return &definitionValue{}, nil })

	bundle := BundleOf(first, second)
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	require.Len(t, entries, 2)
	assert.True(t, pluginmodel.SameDefinition(pluginmodel.Definition(first), entries[0].Definition))
	assert.Contains(t, entries[0].Origin, "declarations_test.go:", "the origin names the composing call site")
	assert.Equal(t, entries[0].Origin, entries[1].Origin)

	combined := CombineBundles(bundle, BundleOf(first))
	combinedEntries := pluginmodel.BundleEntries(pluginmodel.Bundle(combined))
	require.Len(t, combinedEntries, 3, "combining flattens without deduplicating; freezing resolves duplicates")
	assert.NotEqual(t, combinedEntries[0].Origin, combinedEntries[2].Origin,
		"each occurrence keeps the source that introduced it")

	assert.Empty(t, pluginmodel.BundleEntries(pluginmodel.Bundle(CombineBundles())))
	assert.Empty(t, pluginmodel.BundleEntries(pluginmodel.Bundle(BundleOf())))
}

func TestInputsRejectsNilTokensByPosition(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "xbc: plugin.Inputs item 1 is nil", func() {
		Inputs(Collect[definitionContract](), nil)
	})
}

func buildContextFor(t *testing.T, consumer Identity, slots map[uint64][]pluginmodel.ResolvedEntry) BuildContext {
	t.Helper()
	return BuildContext(pluginmodel.NewBuildContext(
		pluginmodel.Identity{Plugin: consumer.Plugin, Instance: consumer.Instance}, nil, slots))
}

func TestTypedInputsReadPreBoundEntriesWithoutLiveLookup(t *testing.T) {
	t.Parallel()
	ref := RefToInstance[definitionContract]("producer", "readonly")
	one := RequireOne[definitionContract]()
	optional := OptionalOne[definitionContract]()
	many := Collect[definitionContract]()

	producer := pluginmodel.ResolvedEntry{
		Identity: pluginmodel.Identity{Plugin: "producer", Instance: "readonly"},
		Value:    &definitionValue{value: 7},
	}
	other := pluginmodel.ResolvedEntry{
		Identity: pluginmodel.Identity{Plugin: "other", Instance: DefaultInstance},
		Value:    &definitionValue{value: 9},
	}
	context := buildContextFor(t, Identity{Plugin: "consumer"}, map[uint64][]pluginmodel.ResolvedEntry{
		ref.inputToken().ID:      {producer},
		one.inputToken().ID:      {producer},
		optional.inputToken().ID: nil,
		many.inputToken().ID:     {producer, other},
	})

	assert.Equal(t, Identity{Plugin: "consumer", Instance: DefaultInstance}, context.Identity())
	assert.NotNil(t, context.Log())

	entry := ref.Get(context)
	assert.Equal(t, Identity{Plugin: "producer", Instance: "readonly"}, entry.Identity)
	assert.Equal(t, 7, entry.Value.Value())
	assert.Equal(t, 7, one.Get(context).Value.Value())

	_, found := optional.Get(context)
	assert.False(t, found, "an unbound optional reports absence rather than a zero producer")

	collected := many.Get(context)
	require.Len(t, collected, 2)
	assert.Equal(t, 7, collected[0].Value.Value())
	assert.Equal(t, Key("other"), collected[1].Identity.Plugin)
}

func TestTypedInputsPanicWhenTheirBindingViolatesTheDeclaredCardinality(t *testing.T) {
	t.Parallel()
	ref := RefTo[definitionContract]("producer")
	optional := OptionalOne[definitionContract]()
	bound := pluginmodel.ResolvedEntry{
		Identity: pluginmodel.Identity{Plugin: "producer"},
		Value:    &definitionValue{},
	}
	context := buildContextFor(t, Identity{Plugin: "consumer"}, map[uint64][]pluginmodel.ResolvedEntry{
		ref.inputToken().ID:      nil,
		optional.inputToken().ID: {bound, bound},
	})

	assert.Panics(t, func() { ref.Get(context) }, "a Ref bound to no producer is an internal invariant violation")
	assert.Panics(t, func() { optional.Get(context) }, "an Optional bound twice is an internal invariant violation")
}

func TestTypedInputsPanicWhenABindingHasTheWrongType(t *testing.T) {
	t.Parallel()
	input := RefTo[definitionContract]("producer")
	context := buildContextFor(t, Identity{Plugin: "consumer"}, map[uint64][]pluginmodel.ResolvedEntry{
		input.inputToken().ID: {{Identity: pluginmodel.Identity{Plugin: "producer"}, Value: 42}},
	})
	assert.PanicsWithValue(t,
		"xbc: internal invariant: input token "+strconv.FormatUint(input.inputToken().ID, 10)+" bound int, want plugin.definitionContract",
		func() { input.Get(context) })
}

func TestDefinePlannedSelectsInputsFromPreparedConfiguration(t *testing.T) {
	t.Parallel()
	type config struct{ Enabled bool }
	input := Collect[definitionContract]()
	definition := DefinePlanned("planned", ConfigSpec[config]{
		Defaults: func() config { return config{} },
	}, func(value config) (Plan[*definitionValue], error) {
		if !value.Enabled {
			return PlanOf(Inputs(), func(BuildContext) (*definitionValue, error) {
				return &definitionValue{value: 1}, nil
			}), nil
		}
		return PlanOf(Inputs(input), func(context BuildContext) (*definitionValue, error) {
			return &definitionValue{value: len(input.Get(context))}, nil
		}), nil
	})

	descriptor := descriptorOf(t, definition)
	require.NotNil(t, descriptor.Plan)

	plan, err := descriptor.Plan(config{})
	require.NoError(t, err)
	assert.Empty(t, plan.Inputs)

	plan, err = descriptor.Plan(config{Enabled: true})
	require.NoError(t, err)
	require.Len(t, plan.Inputs, 1)
	assert.Equal(t, input.inputToken().ID, plan.Inputs[0].ID)
}

func TestPlannerFailuresAreReportedWithTheOwningKey(t *testing.T) {
	t.Parallel()
	type config struct{}
	failing := DefinePlanned("failing", ConfigSpec[config]{
		Defaults: func() config { return config{} },
	}, func(config) (Plan[*definitionValue], error) {
		return Plan[*definitionValue]{}, errors.New("planner refused")
	})
	_, err := descriptorOf(t, failing).Plan(config{})
	assert.EqualError(t, err, "planner refused")

	empty := DefinePlanned("empty", ConfigSpec[config]{
		Defaults: func() config { return config{} },
	}, func(config) (Plan[*definitionValue], error) { return Plan[*definitionValue]{}, nil })
	_, err = descriptorOf(t, empty).Plan(config{})
	assert.EqualError(t, err, "xbc: plugin empty planner returned a nil factory")

	_, err = descriptorOf(t, empty).Plan("not the config type")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prepared config for plugin empty has type string")

	configured := DefineConfigured("configured", ConfigSpec[config]{
		Defaults: func() config { return config{} },
		Prepare:  func(value config) (config, error) { return value, nil },
	}, func(BuildContext, config) (*definitionValue, error) { return &definitionValue{}, nil })
	descriptor := descriptorOf(t, configured)
	_, err = descriptor.Plan(42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prepared config for plugin configured has type int")
	_, err = descriptor.Config.Prepare(42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config has type int")
}

func TestDefinitionOriginNamesTheDeclaringCallSite(t *testing.T) {
	t.Parallel()
	definition := Define("origin", func(BuildContext) (*definitionValue, error) { return &definitionValue{}, nil })
	descriptor := descriptorOf(t, definition)
	assert.True(t, strings.Contains(descriptor.Origin, "declarations_test.go:"), descriptor.Origin)
	assert.Equal(t, typeOf[*definitionValue](), descriptor.Primary)
	assert.Equal(t, pluginmodel.SingleInstance, descriptor.Cardinality)
	assert.Equal(t, pluginmodel.ActivationAlways, descriptor.Activation.Kind)
	assert.Nil(t, descriptor.Config, "an unconfigured Definition carries no config descriptor")
}

func TestEraseLifecycleAdaptsEveryStageToTheOwningPrimaryType(t *testing.T) {
	t.Parallel()
	var stages []string
	record := func(stage string) func(*definitionValue, *Context) error {
		return func(value *definitionValue, ctx *Context) error {
			stages = append(stages, stage+":"+ctx.Name())
			value.value++
			return nil
		}
	}
	definition := Define("staged", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{}, nil
	}, Options[*definitionValue]{Lifecycle: Lifecycle[*definitionValue]{
		Init:        record("init"),
		Migrate:     record("migrate"),
		Start:       record("start"),
		OpenTraffic: record("open"),
		Stop: func(value *definitionValue, ctx context.Context) error {
			stages = append(stages, "stop")
			return ctx.Err()
		},
	}})

	adapters := descriptorOf(t, definition).Lifecycle
	value := &definitionValue{}
	ctx := NewRuntimeContext(newFakeHost(), Identity{Plugin: "staged"})
	require.NoError(t, adapters.Init(value, ctx))
	require.NoError(t, adapters.Migrate(value, ctx))
	require.NoError(t, adapters.Start(value, ctx))
	require.NoError(t, adapters.OpenTraffic(value, ctx))
	require.NoError(t, adapters.Stop(value, context.Background()))
	assert.Equal(t, []string{"init:staged", "migrate:staged", "start:staged", "open:staged", "stop"}, stages)
	assert.Equal(t, 4, value.value)

	bare := descriptorOf(t, Define("bare", func(BuildContext) (*definitionValue, error) {
		return &definitionValue{}, nil
	})).Lifecycle
	assert.Nil(t, bare.Init)
	assert.Nil(t, bare.Stop)
}
