package assembly

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// timingProbe is the shortest wait that survives a coarse clock without
// slowing the suite down. Assertions compare against it with >=, never with
// equality: a scheduler is free to make any stage take longer.
const timingProbe = 20 * time.Millisecond

func timingStages(timings []StageTiming) []Stage {
	stages := make([]Stage, len(timings))
	for index, timing := range timings {
		stages[index] = timing.Stage
	}
	return stages
}

func timingFor(t *testing.T, timings []StageTiming, stage Stage) time.Duration {
	t.Helper()
	for _, timing := range timings {
		if timing.Stage == stage {
			return timing.Duration
		}
	}
	t.Fatalf("no timing was recorded for stage %s", stage)
	return 0
}

// TestTimingsRecordEveryDeclaredStageInTheOrderItRan is what turns "the boot
// took 40 seconds" into a plugin and a stage. The order assertion matters as
// much as the durations: a reader uses this sequence to reconstruct what the
// framework did to one value, so a set would not do.
func TestTimingsRecordEveryDeclaredStageInTheOrderItRan(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("slow", func(plugin.BuildContext) (*store, error) {
		time.Sleep(timingProbe)
		return &store{name: "slow"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Init: func(*store, *plugin.Context) error {
			time.Sleep(timingProbe)
			return nil
		},
		Migrate:     func(*store, *plugin.Context) error { return nil },
		Start:       func(*store, *plugin.Context) error { return nil },
		OpenTraffic: func(*store, *plugin.Context) error { return nil },
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "slow"})
	require.True(t, ok)

	assert.Equal(t, []Stage{StageFactory, StageInit}, timingStages(instance.Timings()),
		"only the stages Construct has actually run are recorded when it returns")
	assert.GreaterOrEqual(t, timingFor(t, instance.Timings(), StageFactory), timingProbe,
		"a factory that does slow work is attributed to its own plugin, not folded into Init")
	assert.GreaterOrEqual(t, timingFor(t, instance.Timings(), StageInit), timingProbe)

	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())

	assert.Equal(t,
		[]Stage{StageFactory, StageInit, StageMigrate, StageStart, StageOpenTraffic},
		timingStages(instance.Timings()))
}

// TestTimingsOmitAStageTheDefinitionNeverDeclared keeps "did not run"
// distinguishable from "ran instantly". Every Invoke* method is called on an
// instance that declares no hook at all, because the runtime's Has* guard is
// not the only caller and a zero-length entry would read as a stage that ran.
func TestTimingsOmitAStageTheDefinitionNeverDeclared(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("bare-timing", func(plugin.BuildContext) (*store, error) {
		return &store{name: "bare-timing"}, nil
	})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "bare-timing"})
	require.True(t, ok)

	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())

	assert.Equal(t, []Stage{StageFactory}, timingStages(instance.Timings()),
		"a factory always runs; an undeclared hook produces no timing")
}

// TestTimingsRecordAStageThatFailed pins the reading an operator needs most:
// "Start failed after 30 seconds" is a different diagnosis from "Start failed
// immediately", and the error text carries neither.
func TestTimingsRecordAStageThatFailed(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("slow-failure", func(plugin.BuildContext) (*store, error) {
		return &store{name: "slow-failure"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Start: func(*store, *plugin.Context) error {
			time.Sleep(timingProbe)
			return errors.New("dial timeout")
		},
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "slow-failure"})
	require.True(t, ok)

	require.Error(t, instance.InvokeStart())
	assert.GreaterOrEqual(t, timingFor(t, instance.Timings(), StageStart), timingProbe,
		"a stage that ended in an error still spent the time it spent")
}

// TestShutdownRecordsAttributeTheBudgetToWhatConsumedIt closes the other half.
// The existing warning names the plugins the budget cut off; without a
// duration per attempt nothing names the plugin that spent the budget, which
// is the one an operator has to fix.
func TestShutdownRecordsAttributeTheBudgetToWhatConsumedIt(t *testing.T) {
	t.Parallel()
	slow := plugin.Define("a-slow-stop", func(plugin.BuildContext) (*store, error) {
		return &store{name: "a-slow-stop"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error {
			time.Sleep(timingProbe)
			return nil
		},
	}})
	quiet := plugin.Define("z-no-stop", func(plugin.BuildContext) (*store, error) {
		return &store{name: "z-no-stop"}, nil
	})
	plan, err := planFor(t, nil, slow, quiet)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := constructed.Unwind(deadline, time.Minute, nil)
	require.NoError(t, err)

	durations := make(map[plugin.Key]time.Duration, len(report.Records))
	for _, record := range report.Records {
		durations[record.Identity.Plugin] = record.Duration
	}
	assert.GreaterOrEqual(t, durations["a-slow-stop"], timingProbe)
	assert.Zero(t, durations["z-no-stop"], "there was nothing to wait for, so nothing was waited for")
}

// TestStageBeginAnnouncesEveryStageBeforeItRuns is the observation Timings
// cannot provide. A timing entry is appended after the hook returns, so the
// stage a caller most needs named -- the one that has not returned -- has no
// entry at all. This callback is the only thing that names it, and it is only
// usable if it fires strictly before the hook body: the interleaving assertion,
// not the count, is what pins that.
func TestStageBeginAnnouncesEveryStageBeforeItRuns(t *testing.T) {
	t.Parallel()
	var events []string
	record := func(event string) { events = append(events, event) }
	lifecycle := func(key string) plugin.Lifecycle[*store] {
		return plugin.Lifecycle[*store]{
			Init:        func(*store, *plugin.Context) error { record("hook " + key + " Init"); return nil },
			Migrate:     func(*store, *plugin.Context) error { record("hook " + key + " Migrate"); return nil },
			Start:       func(*store, *plugin.Context) error { record("hook " + key + " Start"); return nil },
			OpenTraffic: func(*store, *plugin.Context) error { record("hook " + key + " OpenTraffic"); return nil },
			Stop:        func(*store, context.Context) error { record("hook " + key + " Stop"); return nil },
		}
	}
	first := plugin.Define("a-first", func(plugin.BuildContext) (*store, error) {
		record("hook a-first factory")
		return &store{name: "a-first"}, nil
	}, plugin.Options[*store]{Lifecycle: lifecycle("a-first")})
	second := plugin.Define("b-second", func(plugin.BuildContext) (*store, error) {
		record("hook b-second factory")
		return &store{name: "b-second"}, nil
	}, plugin.Options[*store]{Lifecycle: lifecycle("b-second")})

	plan, err := planFor(t, nil, first, second)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{
		OnStageBegin: func(identity plugin.Identity, stage Stage) {
			record("begin " + identity.String() + " " + string(stage))
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"begin a-first factory", "hook a-first factory",
		"begin a-first Init", "hook a-first Init",
		"begin b-second factory", "hook b-second factory",
		"begin b-second Init", "hook b-second Init",
	}, events, "each stage is announced once, immediately before it runs, in graph order")

	events = nil
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "a-first"})
	require.True(t, ok)
	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())

	assert.Equal(t, []string{
		"begin a-first Migrate", "hook a-first Migrate",
		"begin a-first Start", "hook a-first Start",
		"begin a-first OpenTraffic", "hook a-first OpenTraffic",
	}, events, "the stages the runtime drives after construction are announced the same way")

	events = nil
	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = constructed.Unwind(deadline, time.Minute, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"hook b-second Stop", "hook a-first Stop"}, events,
		"Stop announces nothing: it is bounded, reported per attempt, and already observable while in flight")
}

// TestStageBeginIsNotAnnouncedForAnUndeclaredHook keeps the callback saying
// what it claims. A stage that never runs must not be announced as beginning,
// or the last announcement would name a plugin that is doing nothing -- which
// is precisely the reading the callback exists to make trustworthy.
func TestStageBeginIsNotAnnouncedForAnUndeclaredHook(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("bare-begin", func(plugin.BuildContext) (*store, error) {
		return &store{name: "bare-begin"}, nil
	})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	var stages []Stage
	constructed, err := Construct(plan, ConstructOptions{
		OnStageBegin: func(_ plugin.Identity, stage Stage) { stages = append(stages, stage) },
	})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "bare-begin"})
	require.True(t, ok)
	require.NoError(t, instance.InvokeMigration())
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())

	assert.Equal(t, []Stage{StageFactory}, stages,
		"a factory always runs; an undeclared hook announces nothing")
}

// TestConstructWithoutStageObservationBehavesIdentically pins that the
// observation is optional. Every other test in this package constructs with no
// callback at all, so a construction path that depended on one would fail
// everywhere; this states the contract where a reader can find it.
func TestConstructWithoutStageObservationBehavesIdentically(t *testing.T) {
	t.Parallel()
	definition := plugin.Define("unobserved", func(plugin.BuildContext) (*store, error) {
		return &store{name: "unobserved"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Init:  func(*store, *plugin.Context) error { return nil },
		Start: func(*store, *plugin.Context) error { return nil },
	}})
	plan, err := planFor(t, nil, definition)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "unobserved"})
	require.True(t, ok)
	require.NoError(t, instance.InvokeStart())

	assert.Equal(t, []Stage{StageFactory, StageInit, StageStart}, timingStages(instance.Timings()),
		"an unobserved construction runs and times exactly the same stages")
}
