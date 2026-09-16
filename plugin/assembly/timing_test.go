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
