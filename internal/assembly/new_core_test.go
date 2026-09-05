package assembly

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
)

func testEnvironment(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(values, "XBC_TEST_UNSET_")
	require.NoError(t, err)
	return environment
}

func TestBuildPlanBundleIdentityAndCollision(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("shared", func(plugin.BuildContext) (*int, error) {
		value := 1
		return &value, nil
	})
	bundle := plugin.BundleOf(definition)

	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle, bundle}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{{Plugin: "shared", Instance: plugin.DefaultInstance}}, plan.Order())

	other := plugin.Define("shared", func(plugin.BuildContext) (*int, error) {
		value := 2
		return &value, nil
	})
	_, err = BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle, plugin.BundleOf(other)}, Env: testEnvironment(t, nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different Definition handles")
	assert.Contains(t, err.Error(), "first:")
	assert.Contains(t, err.Error(), "second:")
}

type coreContract interface{ Number() int }

type coreValue struct {
	number int
	stops  *[]int
}

func (value *coreValue) Number() int { return value.number }
func (value *coreValue) Stop(context.Context) error {
	if value.stops != nil {
		*value.stops = append(*value.stops, value.number)
	}
	return nil
}

func TestBuildPlanDoesNotInvokeFactoriesAndConstructWiresTypedInputs(t *testing.T) {
	t.Parallel()
	var factories atomic.Int32
	producer := plugin.Define("producer", func(plugin.BuildContext) (*coreValue, error) {
		factories.Add(1)
		return &coreValue{number: 42}, nil
	}, plugin.Options[*coreValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *coreValue) coreContract { return value })),
	})
	input := plugin.RefTo[coreContract]("producer")
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*int, error) {
		factories.Add(1)
		value := input.Get(context)
		if value.Identity.Plugin != "producer" {
			return nil, fmt.Errorf("wrong identity: %s", value.Identity)
		}
		result := value.Value.Number()
		return &result, nil
	}, plugin.Options[*int]{Inputs: plugin.Inputs(input)})

	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(consumer, producer)}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	assert.Zero(t, factories.Load(), "planning must not invoke a factory")
	assert.Equal(t, []plugin.Identity{{Plugin: "producer", Instance: "default"}, {Plugin: "consumer", Instance: "default"}}, plan.Order())

	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), factories.Load())
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "consumer", Instance: "default"})
	require.True(t, ok)
	assert.Equal(t, 42, *instance.Primary().(*int))
}

func TestBuildPlanReportsMissingAmbiguousOptionalAndManySelf(t *testing.T) {
	t.Parallel()
	one := plugin.RequireOne[coreContract]()
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*int, error) {
		_ = one.Get(context)
		return new(int), nil
	}, plugin.Options[*int]{Inputs: plugin.Inputs(one)})
	_, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(consumer)}, Env: testEnvironment(t, nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found none")

	makeProducer := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*coreValue, error) { return &coreValue{}, nil }, plugin.Options[*coreValue]{
			Exports: plugin.Contracts(plugin.ExportAs(func(value *coreValue) coreContract { return value })),
		})
	}
	_, err = BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(consumer, makeProducer("a"), makeProducer("b"))}, Env: testEnvironment(t, nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "a, b")

	optional := plugin.OptionalOne[coreContract]()
	optionalConsumer := plugin.Define("optional", func(context plugin.BuildContext) (*int, error) {
		_, found := optional.Get(context)
		if found {
			return nil, errors.New("unexpected optional")
		}
		return new(int), nil
	}, plugin.Options[*int]{Inputs: plugin.Inputs(optional)})
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(optionalConsumer)}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	many := plugin.Collect[coreContract]()
	self := plugin.Define("self", func(context plugin.BuildContext) (*coreValue, error) {
		_ = many.Get(context)
		return &coreValue{}, nil
	}, plugin.Options[*coreValue]{
		Inputs:  plugin.Inputs(many),
		Exports: plugin.Contracts(plugin.ExportAs(func(value *coreValue) coreContract { return value })),
	})
	_, err = BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(self)}, Env: testEnvironment(t, nil)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "self-dependency")
}

func TestConfiguredOrderDefaultsBindValidatePreparePlan(t *testing.T) {
	t.Parallel()
	type cfg struct {
		Value string `yaml:"value" validate:"required"`
	}
	var events []string
	definition := plugin.DefinePlanned("configured", plugin.ConfigSpec[cfg]{
		Defaults: func() cfg {
			events = append(events, "defaults")
			return cfg{Value: "default"}
		},
		Prepare: func(value cfg) (cfg, error) {
			events = append(events, "prepare:"+value.Value)
			value.Value += "-prepared"
			return value, nil
		},
	}, func(value cfg) (plugin.Plan[*string], error) {
		events = append(events, "plan:"+value.Value)
		return plugin.PlanOf(plugin.Inputs(), func(plugin.BuildContext) (*string, error) {
			events = append(events, "factory:"+value.Value)
			result := value.Value
			return &result, nil
		}), nil
	}, plugin.Options[*string]{Activation: plugin.WhenConfigured("plugins.configured")})

	plan, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(definition)},
		Env:     testEnvironment(t, map[string]any{"plugins": map[string]any{"configured": map[string]any{"value": "bound"}}}),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"defaults", "prepare:bound", "plan:bound-prepared"}, events)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"defaults", "prepare:bound", "plan:bound-prepared", "factory:bound-prepared"}, events)
	instance, _ := constructed.Instance(plugin.Identity{Plugin: "configured", Instance: "default"})
	assert.Equal(t, "bound-prepared", *instance.Primary().(*string))
}

func TestCustomConfigPathHasNoConventionalFallback(t *testing.T) {
	t.Parallel()
	type cfg struct {
		Addr string `yaml:"addr"`
	}
	definition := plugin.DefineConfigured("web", plugin.ConfigSpec[cfg]{
		Defaults: func() cfg { return cfg{Addr: ":8080"} },
	}, func(_ plugin.BuildContext, config cfg) (*string, error) {
		return &config.Addr, nil
	}, plugin.Options[*string]{ConfigPath: "web"})

	plan, err := BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(definition)},
		Env:     testEnvironment(t, map[string]any{"web": map[string]any{"addr": ":9000"}}),
	})
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "web", Instance: plugin.DefaultInstance})
	require.True(t, ok)
	assert.Equal(t, ":9000", *instance.Primary().(*string))

	_, err = BuildPlan(PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(definition)},
		Env: testEnvironment(t, map[string]any{
			"plugins": map[string]any{"web": map[string]any{"addr": ":7000"}},
		}),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.web has configuration but no corresponding plugin")
}

func TestConstructRejectsTypedNilInvalidatesContextAndUnwindsOwnedOnInitFailure(t *testing.T) {
	t.Parallel()
	var escaped plugin.BuildContext
	nilDefinition := plugin.Define("nil", func(context plugin.BuildContext) (*coreValue, error) {
		escaped = context
		return nil, nil
	})
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(nilDefinition)}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned nil")
	assert.PanicsWithValue(t, "xbc: plugin nil used BuildContext after its factory returned", func() { _ = escaped.Identity() })

	var stopped []int
	first := plugin.Define("first", func(plugin.BuildContext) (*coreValue, error) {
		return &coreValue{number: 1, stops: &stopped}, nil
	})
	second := plugin.Define("second", func(plugin.BuildContext) (*coreValue, error) {
		return &coreValue{number: 2, stops: &stopped}, nil
	}, plugin.Options[*coreValue]{Lifecycle: plugin.Lifecycle[*coreValue]{
		Init: func(*coreValue, *plugin.Context) error { return errors.New("broken init") },
	}})
	plan, err = BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(first, second)}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken init")
	assert.Equal(t, []int{2, 1}, stopped, "ownership transfers before Init and unwind is reverse graph order")
}

func TestConcurrentPlansShareTokenWithoutBindingState(t *testing.T) {
	t.Parallel()
	input := plugin.RefTo[*int]("producer")
	producer := plugin.Define("producer", func(context plugin.BuildContext) (*int, error) {
		value := 7
		return &value, nil
	})
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*int, error) {
		value := *input.Get(context).Value
		return &value, nil
	}, plugin.Options[*int]{Inputs: plugin.Inputs(input)})
	bundle := plugin.BundleOf(producer, consumer)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{bundle}, Env: testEnvironment(t, nil)})
			assert.NoError(t, err)
			constructed, err := Construct(plan, ConstructOptions{})
			assert.NoError(t, err)
			instance, ok := constructed.Instance(plugin.Identity{Plugin: "consumer", Instance: "default"})
			assert.True(t, ok)
			assert.Equal(t, 7, *instance.Primary().(*int))
		}()
	}
	wg.Wait()
}

func TestStopErrorsAndPanicsDoNotTruncateUnwind(t *testing.T) {
	t.Parallel()
	type stopValue struct{ key string }
	var stopped []string
	makeDefinition := func(key plugin.Key, behavior string) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*stopValue, error) {
			return &stopValue{key: key.String()}, nil
		}, plugin.Options[*stopValue]{Lifecycle: plugin.Lifecycle[*stopValue]{
			Stop: func(value *stopValue, _ context.Context) error {
				stopped = append(stopped, value.key)
				switch behavior {
				case "panic":
					panic("stop panic")
				case "error":
					return errors.New("stop error")
				}
				return nil
			},
		}})
	}
	plan, err := BuildPlan(PlanOptions{Bundles: []plugin.Bundle{plugin.BundleOf(
		makeDefinition("a", ""), makeDefinition("b", "error"), makeDefinition("c", "panic"),
	)}, Env: testEnvironment(t, nil)})
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = constructed.Unwind(deadline, time.Second, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop panic")
	assert.Contains(t, err.Error(), "stop error")
	assert.Equal(t, []string{"c", "b", "a"}, stopped)
}
