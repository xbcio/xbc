package assembly

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type storeContract interface{ Name() string }

type store struct{ name string }

func (value *store) Name() string { return value.name }

func storeDefinition(key plugin.Key, instances plugin.Cardinality) plugin.Definition {
	return plugin.Define(key, func(context plugin.BuildContext) (*store, error) {
		return &store{name: context.Identity().String()}, nil
	}, plugin.Options[*store]{
		Instances: instances,
		Exports:   plugin.Contracts(plugin.ExportAs(func(value *store) storeContract { return value })),
	})
}

func TestRefResolutionTargetsOneEnabledExporter(t *testing.T) {
	t.Parallel()
	producer := storeDefinition("gorm", plugin.MultipleInstances)
	named := plugin.RefToInstance[storeContract]("gorm", "readonly")
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*store, error) {
		return &store{name: named.Get(context).Value.Name()}, nil
	}, plugin.Options[*store]{Inputs: plugin.Inputs(named)})

	values := map[string]any{"plugins": map[string]any{"gorm": map[string]any{
		"readonly": map[string]any{},
		"primary":  map[string]any{},
	}}}
	plan, err := planFor(t, values, producer, consumer)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "consumer"})
	require.True(t, ok)
	assert.Equal(t, "gorm[readonly]", instance.Primary().(*store).Name(),
		"a Ref binds the exact named instance, not a same-key sibling")

	_, err = planFor(t, nil, producer, consumer)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"xbc: plugin consumer requires assembly.storeContract from exact producer gorm[readonly], but it is not enabled or does not export that contract")

	other := plugin.Define("other", func(plugin.BuildContext) (*validationValue, error) {
		return &validationValue{}, nil
	})
	wrongContract := plugin.RefTo[storeContract]("other")
	mismatched := plugin.Define("mismatched", func(context plugin.BuildContext) (*store, error) {
		return &store{name: wrongContract.Get(context).Value.Name()}, nil
	}, plugin.Options[*store]{Inputs: plugin.Inputs(wrongContract)})
	_, err = planFor(t, nil, other, mismatched)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not export that contract")
}

func TestCollectBindsEveryExporterInGraphOrderAndToleratesNone(t *testing.T) {
	t.Parallel()
	all := plugin.Collect[storeContract]()
	var seen []string
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*validationValue, error) {
		for _, entry := range all.Get(context) {
			seen = append(seen, entry.Identity.String())
		}
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Inputs: plugin.Inputs(all)})

	plan, err := planFor(t, nil, consumer)
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	assert.Empty(t, seen, "an empty Collect is valid")

	plan, err = planFor(t, map[string]any{"plugins": map[string]any{"gorm": map[string]any{
		"readonly": map[string]any{},
		"primary":  map[string]any{},
	}}}, consumer, storeDefinition("gorm", plugin.MultipleInstances), storeDefinition("redis", plugin.SingleInstance))
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{
		{Plugin: "gorm", Instance: "primary"},
		{Plugin: "gorm", Instance: "readonly"},
		{Plugin: "redis", Instance: plugin.DefaultInstance},
		{Plugin: "consumer", Instance: plugin.DefaultInstance},
	}, plan.Order(), "producers precede their consumer and siblings keep canonical order")
	assert.Equal(t, plan.Order()[:3], plan.Contracts(reflect.TypeOf((*storeContract)(nil)).Elem()))
	assert.Empty(t, plan.Contracts(reflect.TypeOf(0)))

	seen = nil
	_, err = Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"gorm[primary]", "gorm[readonly]", "redis"}, seen)
}

func TestOptionalRejectsAmbiguityButAcceptsAbsence(t *testing.T) {
	t.Parallel()
	optional := plugin.OptionalOne[storeContract]()
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*validationValue, error) {
		_, _ = optional.Get(context)
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Inputs: plugin.Inputs(optional)})

	_, err := planFor(t, nil, consumer, storeDefinition("gorm", plugin.SingleInstance), storeDefinition("redis", plugin.SingleInstance))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin consumer input assembly.storeContract is ambiguous; candidates: gorm, redis")

	plan, err := planFor(t, nil, consumer, storeDefinition("gorm", plugin.SingleInstance))
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{
		{Plugin: "gorm", Instance: plugin.DefaultInstance},
		{Plugin: "consumer", Instance: plugin.DefaultInstance},
	}, plan.Order())
}

func TestDependencyCyclesAreNamedRatherThanDeadlocked(t *testing.T) {
	t.Parallel()
	toSecond := plugin.RefTo[*store]("second")
	first := plugin.Define("first", func(context plugin.BuildContext) (*store, error) {
		return &store{name: toSecond.Get(context).Value.Name()}, nil
	}, plugin.Options[*store]{Inputs: plugin.Inputs(toSecond)})
	toFirst := plugin.RefTo[*store]("first")
	second := plugin.Define("second", func(context plugin.BuildContext) (*store, error) {
		return &store{name: toFirst.Get(context).Value.Name()}, nil
	}, plugin.Options[*store]{Inputs: plugin.Inputs(toFirst)})

	_, err := planFor(t, nil, first, second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin dependency cycle involves first, second")
}

func TestNilPlanAndUnknownIdentityAreRejectedWithoutPanicking(t *testing.T) {
	t.Parallel()
	_, err := Construct(nil, ConstructOptions{})
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: cannot construct a nil assembly plan")

	var missing *Constructed
	assert.Nil(t, missing.Instances())
	_, ok := missing.Instance(plugin.Identity{Plugin: "absent"})
	assert.False(t, ok)
	require.NoError(t, missing.Unwind(context.Background(), time.Second, nil))

	plan, err := planFor(t, nil, storeDefinition("gorm", plugin.SingleInstance))
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	_, ok = constructed.Instance(plugin.Identity{Plugin: "absent"})
	assert.False(t, ok)
	_, ok = constructed.Instance(plugin.Identity{Plugin: "gorm", Instance: ""})
	assert.True(t, ok, "lookup normalizes the instance name")
	require.Len(t, constructed.Instances(), 1)
}

func TestFactoryFailuresAndInvalidPrimariesAreNamedAndFullyUnwound(t *testing.T) {
	t.Parallel()
	var stopped []string
	survivor := plugin.Define("survivor", func(plugin.BuildContext) (*store, error) {
		return &store{name: "survivor"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(value *store, _ context.Context) error {
			stopped = append(stopped, value.name)
			return nil
		},
	}})

	for name, testCase := range map[string]struct {
		definition plugin.Definition
		message    string
	}{
		"error": {
			definition: plugin.Define("zzz-broken", func(plugin.BuildContext) (*store, error) {
				return nil, errors.New("no connection")
			}),
			message: "xbc: plugin zzz-broken factory failed: no connection",
		},
		"panic": {
			definition: plugin.Define("zzz-broken", func(plugin.BuildContext) (*store, error) {
				panic("factory boom")
			}),
			message: "xbc: plugin zzz-broken factory panic: factory boom",
		},
		"typed nil": {
			definition: plugin.Define("zzz-broken", func(plugin.BuildContext) (*store, error) {
				return nil, nil
			}),
			message: "xbc: plugin zzz-broken factory returned nil primary value",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stopped = nil
			plan, err := planFor(t, nil, survivor, testCase.definition)
			require.NoError(t, err)
			_, err = Construct(plan, ConstructOptions{ShutdownTimeout: time.Second})
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
			assert.Equal(t, []string{"survivor"}, stopped,
				"every already-owned value is stopped before Construct returns")
		})
	}
}

func TestFactoryReturningTheWrongConcreteTypeViolatesTheFrozenInvariant(t *testing.T) {
	t.Parallel()
	descriptor := pluginmodel.DefinitionDescriptor{
		Key:     "mismatch",
		Primary: reflect.TypeOf(&store{}),
		Plan: func(any) (pluginmodel.InstancePlan, error) {
			return pluginmodel.InstancePlan{
				Factory: func(pluginmodel.BuildContext) (any, error) { return &validationValue{}, nil },
			}, nil
		},
	}
	plan, err := planFor(t, nil, plugin.Definition(pluginmodel.NewDefinition(descriptor)))
	require.NoError(t, err)
	_, err = Construct(plan, ConstructOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: internal invariant: plugin mismatch factory returned *assembly.validationValue, frozen primary type is *assembly.store")
}

type stagedValue struct {
	stages *[]string
	fail   string
	panics string
}

func (value *stagedValue) record(stage string) error {
	*value.stages = append(*value.stages, stage)
	if value.panics == stage {
		panic(stage + " boom")
	}
	if value.fail == stage {
		return errors.New(stage + " refused")
	}
	return nil
}

func (value *stagedValue) Init(*plugin.Context) error        { return value.record("init") }
func (value *stagedValue) Migrate(*plugin.Context) error     { return value.record("migrate") }
func (value *stagedValue) Start(*plugin.Context) error       { return value.record("start") }
func (value *stagedValue) OpenTraffic(*plugin.Context) error { return value.record("open") }
func (value *stagedValue) Stop(context.Context) error        { return value.record("stop") }

func stagedInstance(t *testing.T, stages *[]string, fail, panics string) *Instance {
	t.Helper()
	definition := plugin.Define("staged", func(plugin.BuildContext) (*stagedValue, error) {
		return &stagedValue{stages: stages, fail: fail, panics: panics}, nil
	})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{
		ContextFactory: func(identity plugin.Identity, logger log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(nil, identity)
		},
	})
	if fail == "init" || panics == "init" {
		require.Error(t, err)
		return nil
	}
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "staged"})
	require.True(t, ok)
	return instance
}

func TestLifecycleStagesAreInvokedThroughTheCompiledDescriptor(t *testing.T) {
	t.Parallel()
	var stages []string
	instance := stagedInstance(t, &stages, "", "")

	assert.Equal(t, plugin.Identity{Plugin: "staged", Instance: plugin.DefaultInstance}, instance.Identity())
	require.NotNil(t, instance.Context())
	assert.Equal(t, "staged", instance.Context().Name())
	assert.True(t, instance.HasMigration())
	assert.True(t, instance.HasStart())
	assert.True(t, instance.HasTrafficPreparation())
	assert.True(t, instance.HasStop())

	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())
	require.NoError(t, instance.StopBounded(context.Background(), time.Second))
	assert.Equal(t, []string{"init", "migrate", "start", "open", "stop"}, stages)

	require.NoError(t, instance.StopBounded(context.Background(), time.Second))
	assert.Equal(t, []string{"init", "migrate", "start", "open", "stop"}, stages, "Stop runs at most once")
}

func TestAbsentLifecycleStagesAreReportedAndSkipped(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("bare", func(plugin.BuildContext) (*store, error) {
		return &store{name: "bare"}, nil
	})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "bare"})
	require.True(t, ok)

	assert.False(t, instance.HasMigration())
	assert.False(t, instance.HasStart())
	assert.False(t, instance.HasTrafficPreparation())
	assert.False(t, instance.HasStop())
	assert.Nil(t, instance.Context(), "no ContextFactory means no lifecycle Context")
	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())
	require.NoError(t, instance.StopBounded(context.Background(), time.Second))
}

func TestLifecycleFailuresAndPanicsAreWrappedWithIdentityAndStage(t *testing.T) {
	t.Parallel()
	for stage, invoke := range map[string]func(*Instance) error{
		"Migrate":     (*Instance).InvokeMigration,
		"Start":       (*Instance).InvokeStart,
		"OpenTraffic": (*Instance).InvokeTrafficPreparation,
	} {
		short := map[string]string{"Migrate": "migrate", "Start": "start", "OpenTraffic": "open"}[stage]
		t.Run(stage+" error", func(t *testing.T) {
			var stages []string
			err := invoke(stagedInstance(t, &stages, short, ""))
			require.Error(t, err)
			assert.EqualError(t, err, "xbc: plugin staged "+stage+" failed: "+short+" refused")
		})
		t.Run(stage+" panic", func(t *testing.T) {
			var stages []string
			err := invoke(stagedInstance(t, &stages, "", short))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "xbc: plugin staged "+stage+" panic: "+short+" boom")
			assert.Contains(t, err.Error(), "construct_test.go", "the diagnostic keeps the panic stack")
		})
	}

	var stages []string
	assert.Nil(t, stagedInstance(t, &stages, "init", ""), "an Init failure never yields an owned instance")
	assert.Equal(t, []string{"init", "stop"}, stages, "a failed Init is still stopped")
}

func TestStopBoundedAbandonsAStopThatOutlivesTheBudget(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	definition := plugin.Define("stuck", func(plugin.BuildContext) (*store, error) {
		return &store{name: "stuck"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error {
			<-release
			return nil
		},
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	err = constructed.Unwind(expired, 50*time.Millisecond, nil)
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: plugin stuck Stop did not return within shutdown budget 50ms; abandoning it")
}

func TestUnwindReportsAfterStopFailuresAlongsideStopFailures(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("noisy", func(plugin.BuildContext) (*store, error) {
		return &store{name: "noisy"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error { return errors.New("stop refused") },
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	var visited []plugin.Identity
	err = constructed.Unwind(context.Background(), time.Second, func(identity plugin.Identity) error {
		visited = append(visited, identity)
		return errors.New("join refused")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop refused")
	assert.Contains(t, err.Error(), "join refused")
	assert.Equal(t, []plugin.Identity{{Plugin: "noisy", Instance: plugin.DefaultInstance}}, visited,
		"afterStop runs even when Stop failed")
}
