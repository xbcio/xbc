package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// Startup is a sequence of loops -- migrateAll, startAll, prepareTraffic --
// and each one re-checks whether a stop was requested before it touches the
// next plugin. Without those checks a shutdown arriving mid-startup would keep
// acquiring resources it is already committed to releasing, and would expose
// a half-built application to traffic.
//
// The checks are cheap and easy to drop in a refactor, so each one is pinned
// here by its own case. The naming convention below is "abort-provider" ->
// "abort-consumer": the consumer takes the provider's contract as an input, so
// the graph -- not the plugin key -- guarantees the provider runs first.

// abortingStages builds a lifecycle whose stages are recorded, with one stage
// replaced by an action that interrupts startup.
func abortingStages(
	recorder *readinessRecorder,
	key plugin.Key,
	stage string,
	interrupt func(),
) plugin.Lifecycle[*readinessValue] {
	stages := plugin.Lifecycle[*readinessValue]{
		Stop: func(*readinessValue, context.Context) error {
			recorder.record("stop:" + string(key))
			return nil
		},
	}
	hook := func(*readinessValue, *plugin.Context) error {
		recorder.record(stage + ":" + string(key))
		if interrupt != nil {
			interrupt()
		}
		return nil
	}
	switch stage {
	case "migrate":
		stages.Migrate = hook
	case "start":
		stages.Start = hook
	case "open":
		stages.OpenTraffic = hook
	}
	return stages
}

// abortingPair declares a provider that interrupts startup from the named
// stage and a consumer that only records reaching it.
func abortingPair(recorder *readinessRecorder, stage string, interrupt func()) []plugin.Definition {
	dependency := plugin.RefTo[readinessContract]("abort-provider")
	return []plugin.Definition{
		readinessDefinition(recorder, "abort-provider", nil,
			abortingStages(recorder, "abort-provider", stage, interrupt)),
		readinessDefinition(recorder, "abort-consumer", &dependency,
			abortingStages(recorder, "abort-consumer", stage, nil)),
	}
}

// TestStopDuringMigrationAbortsTheRemainingMigrationsAndUnwinds pins the guard
// at the head of migrateAll. Both plugins were already constructed, so both
// must be torn down even though only one of them migrated.
func TestStopDuringMigrationAbortsTheRemainingMigrationsAndUnwinds(t *testing.T) {
	recorder := &readinessRecorder{}
	var app *App
	app = newRuntimeTestApp(abortingPair(recorder, "migrate",
		func() { app.requestStop(stopReasonSignal) })...)

	args := append([]string{"--migrate"}, runtimeTestConfig(t, time.Second)...)
	code, err := app.Execute(context.Background(), args)

	require.Error(t, err, "an aborted startup is a failure, not a clean exit")
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "migration", "the error names the phase that was abandoned")
	assert.Contains(t, err.Error(), stopReasonSignal, "the error names why startup was abandoned")

	events := recorder.snapshot()
	assert.Contains(t, events, "migrate:abort-provider")
	assert.NotContains(t, events, "migrate:abort-consumer",
		"the consumer's Migrate ran after a stop was already requested: %v", events)
	assert.Equal(t, []string{"stop:abort-consumer", "stop:abort-provider"}, events[len(events)-2:],
		"both plugins were constructed, so both are unwound in reverse order")
}

// TestStopDuringStartupAbortsTheRemainingStartsAndUnwinds pins the guard at the
// head of startAll. Reaching it needs the stop to be requested by an earlier
// phase: a Start hook that requests one is caught by the post-Start check
// instead, which runtime_test.go already covers. So a single plugin requests
// the stop from its Migrate, leaving startAll to find it on its first
// iteration.
func TestStopDuringStartupAbortsTheRemainingStartsAndUnwinds(t *testing.T) {
	recorder := &readinessRecorder{}
	var app *App
	stages := abortingStages(recorder, "abort-provider", "migrate",
		func() { app.requestStop(stopReasonSignal) })
	stages.Start = func(*readinessValue, *plugin.Context) error {
		recorder.record("start:abort-provider")
		return nil
	}
	app = newRuntimeTestApp(readinessDefinition(recorder, "abort-provider", nil, stages))

	args := append([]string{"--migrate"}, runtimeTestConfig(t, time.Second)...)
	code, err := app.Execute(context.Background(), args)

	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "startup", "the error names the phase that was abandoned")
	assert.Contains(t, err.Error(), stopReasonSignal)

	events := recorder.snapshot()
	assert.Equal(t, []string{"build:abort-provider", "migrate:abort-provider", "stop:abort-provider"}, events,
		"migration completed, Start was never entered, and the constructed plugin was still unwound")
}

// TestClosedTaskAdmissionAbortsStartupBeforeTheNextStart pins the second guard
// inside startAll, the one that refuses to run a Start whose task admission
// window cannot be opened.
//
// It exists for the window between the loop's own stop check and openStart:
// requestStop closes admission before the flag it sets can be observed again,
// so a stop landing in that window is visible only as a refused admission.
// The test closes admission directly because that is the only deterministic
// way to reproduce that window -- driving it through requestStop would set the
// flag too, and the earlier check would win the race most of the time.
func TestClosedTaskAdmissionAbortsStartupBeforeTheNextStart(t *testing.T) {
	recorder := &readinessRecorder{}
	var app *App
	app = newRuntimeTestApp(abortingPair(recorder, "start",
		func() { app.tasks.closeAdmission() })...)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "startup")

	events := recorder.snapshot()
	assert.Contains(t, events, "start:abort-provider")
	assert.NotContains(t, events, "start:abort-consumer",
		"a Start ran even though its plugin could not be granted task admission: %v", events)
	assert.Equal(t, []string{"stop:abort-consumer", "stop:abort-provider"}, events[len(events)-2:])
}

// TestStopDuringTrafficPreparationAbortsTheRemainingOpensAndUnwinds pins the
// guard at the head of prepareTraffic, the second half of the two-phase
// readiness protocol. The traffic gate must stay shut throughout.
func TestStopDuringTrafficPreparationAbortsTheRemainingOpensAndUnwinds(t *testing.T) {
	recorder := &readinessRecorder{}
	var app *App
	app = newRuntimeTestApp(abortingPair(recorder, "open",
		func() { app.requestStop(stopReasonSignal) })...)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "traffic preparation", "the error names the phase that was abandoned")
	assert.Contains(t, err.Error(), stopReasonSignal)

	events := recorder.snapshot()
	assert.Contains(t, events, "open:abort-provider")
	assert.NotContains(t, events, "open:abort-consumer",
		"the consumer opened traffic after a stop was already requested: %v", events)
	assert.Equal(t, []string{"stop:abort-consumer", "stop:abort-provider"}, events[len(events)-2:])
	assertChannelOpen(t, app.trafficGate, "the traffic gate opened during an aborted startup")
}
