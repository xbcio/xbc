package assembly

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
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

// TestRefBindsByKeyNotByImplementationType pins that a dependency's identity is
// the plugin key and nothing else. "decoy" is backed by the very same concrete
// Go type as "target" and exports the very same contract, so an implementation
// that resolved on either of those would happily bind it -- and an application
// would silently receive a different plugin than the one it named.
func TestRefBindsByKeyNotByImplementationType(t *testing.T) {
	t.Parallel()
	target := storeDefinition("target", plugin.SingleInstance)
	decoy := storeDefinition("decoy", plugin.SingleInstance)
	named := plugin.RefTo[storeContract]("target")
	consumer := plugin.Define("consumer", func(context plugin.BuildContext) (*validationValue, error) {
		assert.Equal(t, "target", named.Get(context).Value.Name(),
			"the ref bound the decoy, which merely shares target's implementation type")
		return &validationValue{}, nil
	}, plugin.Options[*validationValue]{Inputs: plugin.Inputs(named)})

	// Without target, an identical decoy must not stand in for it.
	_, err := planFor(t, nil, decoy, consumer)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"xbc: plugin consumer requires assembly.storeContract from exact producer target, but it is not enabled or does not export that contract")

	// With both present the key still decides, and the decoy is built as an
	// unrelated peer rather than being skipped as a duplicate.
	plan, err := planFor(t, nil, target, decoy, consumer)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "decoy"})
	require.True(t, ok, "the decoy is a plugin in its own right, not a shadow of target")
	assert.Equal(t, "decoy", instance.Primary().(*store).Name())
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
	unwound, err := missing.Unwind(context.Background(), time.Second, nil)
	require.NoError(t, err)
	assert.Empty(t, unwound.Records)

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

// TestStopBoundedAbandonsAStopThatOutlivesTheShutdownBudget pins the single-instance
// half of the shutdown contract deterministically: with an already-expired
// deadline and a Stop that never returns, only the abandonment branch can be
// taken, so the outcome cannot depend on which of two ready select cases the
// scheduler happens to pick.
func TestStopBoundedAbandonsAStopThatOutlivesTheShutdownBudget(t *testing.T) {
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
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "stuck"})
	require.True(t, ok)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := instance.stopBounded(expired, 50*time.Millisecond)
	assert.Equal(t, StopAbandoned, outcome)
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: plugin stuck Stop did not return within shutdown budget 50ms; abandoning it")
}

// TestUnwindStartsNoStopOnceTheSharedBudgetIsSpent pins the §6.2 ruling. The
// last instance's Stop hangs and burns the whole budget; the two instances
// below it in the graph must then be reported not-attempted and their Stop
// bodies must never be entered.
//
// The `entered` counter is what gives this discriminating power: the previous
// implementation kept walking after the budget expired, so both bodies ran
// (concurrently with the abandoned one, which is why reverse order was a
// scheduling outcome). A report-only assertion would not have caught that.
func TestUnwindStartsNoStopOnceTheSharedBudgetIsSpent(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	stuckEntered := make(chan struct{})
	var entered atomic.Int32
	quiet := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*store, error) {
			return &store{name: key.String()}, nil
		}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
			Stop: func(*store, context.Context) error {
				entered.Add(1)
				return nil
			},
		}})
	}
	stuck := plugin.Define("c-stuck", func(plugin.BuildContext) (*store, error) {
		return &store{name: "c-stuck"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error {
			entered.Add(1)
			close(stuckEntered)
			<-release
			return nil
		},
	}})
	plan, err := planFor(t, nil, quiet("a-first"), quiet("b-second"), stuck)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	report, err := constructed.Unwind(deadline, 40*time.Millisecond, nil)
	<-stuckEntered

	first := plugin.Identity{Plugin: "a-first", Instance: plugin.DefaultInstance}
	second := plugin.Identity{Plugin: "b-second", Instance: plugin.DefaultInstance}
	stuckIdentity := plugin.Identity{Plugin: "c-stuck", Instance: plugin.DefaultInstance}
	assert.Equal(t, []plugin.Identity{stuckIdentity}, report.Attempted,
		"the walk must stop launching Stop hooks once the shared budget is spent")
	assert.Empty(t, report.Completed)
	assert.Equal(t, []plugin.Identity{stuckIdentity}, report.Identities(StopAbandoned))
	assert.Equal(t, []plugin.Identity{second, first}, report.Identities(StopNotAttempted),
		"the remaining identities are named in reverse graph order")
	assert.Equal(t, int32(1), entered.Load(), "only the stuck Stop body may ever have run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin c-stuck Stop did not return within shutdown budget 40ms")
	assert.Contains(t, err.Error(), "xbc: plugin b-second Stop was not attempted")
	assert.Contains(t, err.Error(), "xbc: plugin a-first Stop was not attempted")
}

// TestUnwindCompletesEveryStopSeriallyInReverseGraphOrder pins the normal
// path: Attempted and Completed must be the same reverse-order sequence.
// An implementation that launched every Stop concurrently would still produce
// the right Attempted order but would let Completed come back interleaved,
// which the overlap guard below turns into a hard failure rather than a flake.
func TestUnwindCompletesEveryStopSeriallyInReverseGraphOrder(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		bodies  []string
		inside  int
		overlap bool
	)
	slow := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*store, error) {
			return &store{name: key.String()}, nil
		}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
			Stop: func(*store, context.Context) error {
				mu.Lock()
				inside++
				overlap = overlap || inside > 1
				bodies = append(bodies, key.String())
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			},
		}})
	}
	plan, err := planFor(t, nil, slow("a"), slow("b"), slow("c"))
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := constructed.Unwind(deadline, 5*time.Second, nil)
	require.NoError(t, err)

	want := []plugin.Identity{
		{Plugin: "c", Instance: plugin.DefaultInstance},
		{Plugin: "b", Instance: plugin.DefaultInstance},
		{Plugin: "a", Instance: plugin.DefaultInstance},
	}
	assert.Equal(t, want, report.Attempted)
	assert.Equal(t, want, report.Completed, "Stop completions must not be reordered against invocations")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"c", "b", "a"}, bodies)
	assert.False(t, overlap, "two Stop bodies must never be inside the unwind at the same time")
}

// TestUnwindClassifiesPanicsFailuresAndAbsentHooks pins that the report tells
// a recovered panic apart from a returned error without either truncating the
// walk, and that an instance with no Stop hook is still counted as visited.
func TestUnwindClassifiesPanicsFailuresAndAbsentHooks(t *testing.T) {
	t.Parallel()
	bare := plugin.Define("bare-stop", func(plugin.BuildContext) (*store, error) {
		return &store{name: "bare-stop"}, nil
	})
	failing := plugin.Define("failing-stop", func(plugin.BuildContext) (*store, error) {
		return &store{name: "failing-stop"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error { return errors.New("stop refused") },
	}})
	panicking := plugin.Define("panicking-stop", func(plugin.BuildContext) (*store, error) {
		return &store{name: "panicking-stop"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error { panic("stop exploded") },
	}})
	plan, err := planFor(t, nil, bare, failing, panicking)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := constructed.Unwind(deadline, 5*time.Second, nil)
	require.Error(t, err)

	outcomes := make(map[string]StopOutcome, len(report.Records))
	for _, record := range report.Records {
		outcomes[record.Identity.Plugin.String()] = record.Outcome
	}
	assert.Equal(t, map[string]StopOutcome{
		"panicking-stop": StopPanicked,
		"failing-stop":   StopFailed,
		"bare-stop":      StopSkipped,
	}, outcomes)
	assert.Len(t, report.Completed, 3, "a panicking Stop must not truncate the reverse walk")
	assert.Contains(t, err.Error(), "stop exploded")
	assert.Contains(t, err.Error(), "stop refused")
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
	report, err := constructed.Unwind(context.Background(), time.Second, func(identity plugin.Identity) error {
		visited = append(visited, identity)
		return errors.New("join refused")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop refused")
	assert.Contains(t, err.Error(), "join refused")
	assert.Equal(t, []plugin.Identity{{Plugin: "noisy", Instance: plugin.DefaultInstance}}, visited,
		"afterStop runs even when Stop failed")
	require.Len(t, report.Records, 1)
	assert.Equal(t, StopFailed, report.Records[0].Outcome)
	assert.EqualError(t, report.Records[0].TaskErr, "join refused",
		"the task-join failure is recorded against the same identity, under the same budget")
}

// TestUnwindRunTwiceStopsEachOwnedValueExactlyOnce pins that a second unwind —
// what a shutdown signal racing a startup failure produces — is a reported
// no-op rather than a second round of Stop calls. The stop counter is the
// discriminating assertion: asserting only the second report's outcomes would
// still pass if Stop ran again and simply happened to succeed twice.
func TestUnwindRunTwiceStopsEachOwnedValueExactlyOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	counting := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*store, error) {
			return &store{name: key.String()}, nil
		}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
			Stop: func(*store, context.Context) error {
				calls.Add(1)
				return nil
			},
		}})
	}
	plan, err := planFor(t, nil, counting("first"), counting("second"))
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := constructed.Unwind(deadline, 5*time.Second, nil)
	require.NoError(t, err)
	second, err := constructed.Unwind(deadline, 5*time.Second, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(2), calls.Load(), "each owned value's Stop may run only once")
	for _, record := range first.Records {
		assert.Equal(t, StopCompleted, record.Outcome, record.Identity)
	}
	for _, record := range second.Records {
		assert.Equal(t, StopSkipped, record.Outcome, record.Identity)
	}
	assert.Equal(t, first.Attempted, second.Attempted,
		"a repeated unwind still visits the same instances in the same order")
}

// bothReadyDeadline forces the exact interleaving the trailing non-blocking
// read of done exists for. Done() is a method call, and a select evaluates its
// channel operands before it blocks, so making Done() return only *after* the
// Stop result has been delivered guarantees the following select finds both
// cases ready. A real context.WithTimeout reaches that state only when the
// deadline fires in the same instant the Stop returns; this makes that instant
// reproducible instead of waiting for it to occur by luck.
type bothReadyDeadline struct {
	context.Context
	stopReturned <-chan struct{}
	expired      chan struct{}
}

func (deadline bothReadyDeadline) Done() <-chan struct{} {
	<-deadline.stopReturned
	// Give the Stop goroutine time to finish delivering its result before the
	// caller is allowed to observe an expired budget.
	time.Sleep(20 * time.Millisecond)
	return deadline.expired
}

func (deadline bothReadyDeadline) Err() error { return context.DeadlineExceeded }

// TestStopBoundedPrefersADeliveredResultOverAnExpiredShutdownBudget pins root
// cause B on its own: when a Stop has already delivered its result and the
// budget is spent, the outcome must be decided by what the Stop did, not by
// which of two ready select cases the scheduler picks.
//
// Without the trailing non-blocking read this assertion fails on roughly half
// of all iterations, because Go picks uniformly at random among ready select
// cases — a plugin that stopped cleanly would be reported abandoned, skipped
// in Completed, denied its afterStop join, and named in the operator warning.
func TestStopBoundedPrefersADeliveredResultOverAnExpiredShutdownBudget(t *testing.T) {
	t.Parallel()
	stopReturned := make(chan struct{})
	definition := plugin.Define("racing", func(plugin.BuildContext) (*store, error) {
		return &store{name: "racing"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error {
			close(stopReturned)
			return nil
		},
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "racing"})
	require.True(t, ok)

	expired := make(chan struct{})
	close(expired)
	deadline := bothReadyDeadline{
		Context:      context.Background(),
		stopReturned: stopReturned,
		expired:      expired,
	}

	outcome, err := instance.stopBounded(deadline, 50*time.Millisecond)
	require.NoError(t, err, "a Stop that already returned cleanly must not be reported as a failure")
	assert.Equal(t, StopCompleted, outcome,
		"the delivered Stop result must win over an expired budget, not a coin flip")
}
