package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// readinessRecorder collects lifecycle events from several plugins running in
// one App. Stages are serial by contract, but Stop and managed tasks are not,
// so the mutex is what makes an ordering assertion meaningful rather than a
// race detector finding.
type readinessRecorder struct {
	mu     sync.Mutex
	events []string
}

func (recorder *readinessRecorder) record(event string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, event)
}

func (recorder *readinessRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.events...)
}

type readinessValue struct{ name string }

// readinessDefinition declares a plugin that records every stage it reaches.
// Passing a non-nil dependency makes it consume the other plugin's contract,
// which is how ordering is expressed now that there is no declared order field.
type readinessContract interface{ Named() string }

func (value *readinessValue) Named() string { return value.name }

func readinessDefinition(
	recorder *readinessRecorder,
	key plugin.Key,
	dependency *plugin.Ref[readinessContract],
	lifecycle plugin.Lifecycle[*readinessValue],
) plugin.Definition {
	options := plugin.Options[*readinessValue]{
		Exports:   plugin.Contracts(plugin.ExportAs(func(value *readinessValue) readinessContract { return value })),
		Lifecycle: lifecycle,
	}
	if dependency != nil {
		options.Inputs = plugin.Inputs(*dependency)
	}
	return plugin.Define(key, func(context plugin.BuildContext) (*readinessValue, error) {
		if dependency != nil {
			// Reading the input is what makes the edge real at build time; a
			// declared-but-unread input would still order the graph, but this
			// mirrors what a plugin actually does.
			_ = dependency.Get(context).Value.Named()
		}
		recorder.record("build:" + string(key))
		return &readinessValue{name: string(key)}, nil
	}, options)
}

func readinessStages(recorder *readinessRecorder, key plugin.Key) plugin.Lifecycle[*readinessValue] {
	return plugin.Lifecycle[*readinessValue]{
		Start: func(*readinessValue, *plugin.Context) error {
			recorder.record("start:" + string(key))
			return nil
		},
		OpenTraffic: func(*readinessValue, *plugin.Context) error {
			recorder.record("open:" + string(key))
			return nil
		},
		Stop: func(*readinessValue, context.Context) error {
			recorder.record("stop:" + string(key))
			return nil
		},
	}
}

// TestEveryStartCompletesBeforeAnyOpenTrafficBegins is the global readiness
// barrier. Start acquires resources; OpenTraffic exposes them. Interleaving
// the two would let the first plugin accept requests while a later plugin's
// dependencies are still being acquired.
func TestEveryStartCompletesBeforeAnyOpenTrafficBegins(t *testing.T) {
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("barrier-first")
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "barrier-first", nil, readinessStages(recorder, "barrier-first")),
		readinessDefinition(recorder, "barrier-second", &dependency, readinessStages(recorder, "barrier-second")),
	)

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)

	events := recorder.snapshot()
	lastStart, firstOpen := -1, len(events)
	for index, event := range events {
		if strings.HasPrefix(event, "start:") {
			lastStart = index
		}
		if strings.HasPrefix(event, "open:") && index < firstOpen {
			firstOpen = index
		}
	}
	require.NotEqual(t, -1, lastStart)
	require.Less(t, firstOpen, len(events))
	assert.Less(t, lastStart, firstOpen,
		"an OpenTraffic ran before every Start had returned: %v", events)
}

// TestDependencyOrderDecidesStartupAndReverseShutdown pins that the graph, not
// the plugin key, decides the order. "alpha" sorts before "zulu", so a
// key-ordered implementation would produce the opposite result.
func TestDependencyOrderDecidesStartupAndReverseShutdown(t *testing.T) {
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("zulu")
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "zulu", nil, readinessStages(recorder, "zulu")),
		readinessDefinition(recorder, "alpha", &dependency, readinessStages(recorder, "alpha")),
	)

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)

	assert.Equal(t, []string{
		"build:zulu", "build:alpha",
		"start:zulu", "start:alpha",
		"open:zulu", "open:alpha",
		"stop:alpha", "stop:zulu",
	}, recorder.snapshot(),
		"a provider must be built and started before its consumer, and torn down after it")
}

// TestAFailedStartNeverReachesOpenTrafficAndUnwindsWhatStarted pins the other
// half of the barrier: nothing may be exposed once any Start has failed, and
// only what actually started is unwound.
func TestAFailedStartNeverReachesOpenTrafficAndUnwindsWhatStarted(t *testing.T) {
	failure := errors.New("start refused")
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("start-provider")

	failing := readinessStages(recorder, "start-consumer")
	failing.Start = func(*readinessValue, *plugin.Context) error {
		recorder.record("start:start-consumer")
		return failure
	}
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "start-provider", nil, readinessStages(recorder, "start-provider")),
		readinessDefinition(recorder, "start-consumer", &dependency, failing),
	)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.ErrorIs(t, err, failure)
	assert.Contains(t, err.Error(), "start-consumer", "the failing plugin is named, not just the stage")

	events := recorder.snapshot()
	for _, event := range events {
		assert.False(t, strings.HasPrefix(event, "open:"),
			"traffic was opened after a Start failure: %v", events)
	}
	assert.Equal(t, []string{"stop:start-consumer", "stop:start-provider"}, events[len(events)-2:],
		"both plugins already started, so both are stopped in reverse order")
	assertChannelOpen(t, app.trafficGate, "the traffic gate opened despite a failed startup")
}

// TestAFailedOpenTrafficNamesItsPluginAndUnwindsEverything covers the second
// readiness phase: a failure there must not leave the earlier, already-opened
// plugins running.
func TestAFailedOpenTrafficNamesItsPluginAndUnwindsEverything(t *testing.T) {
	failure := errors.New("cannot listen")
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("open-provider")

	failing := readinessStages(recorder, "open-consumer")
	failing.OpenTraffic = func(*readinessValue, *plugin.Context) error { return failure }
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "open-provider", nil, readinessStages(recorder, "open-provider")),
		readinessDefinition(recorder, "open-consumer", &dependency, failing),
	)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.ErrorIs(t, err, failure)
	assert.Contains(t, err.Error(), "open-consumer")

	events := recorder.snapshot()
	assert.Contains(t, events, "open:open-provider", "the earlier plugin did open before the later one failed")
	assert.Equal(t, []string{"stop:open-consumer", "stop:open-provider"}, events[len(events)-2:])
	assertChannelOpen(t, app.trafficGate, "the traffic gate opened despite a failed readiness phase")
}

// TestShutdownAggregatesEveryStopFailureInsteadOfTheFirst pins that one
// misbehaving plugin cannot hide another's teardown failure, and cannot stop
// the remaining plugins from being asked to shut down at all.
func TestShutdownAggregatesEveryStopFailureInsteadOfTheFirst(t *testing.T) {
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("stop-provider")

	provider := readinessStages(recorder, "stop-provider")
	provider.Stop = func(*readinessValue, context.Context) error {
		recorder.record("stop:stop-provider")
		return errors.New("provider stop failed")
	}
	consumer := readinessStages(recorder, "stop-consumer")
	consumer.Stop = func(*readinessValue, context.Context) error {
		recorder.record("stop:stop-consumer")
		panic(errors.New("consumer stop exploded"))
	}
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "stop-provider", nil, provider),
		readinessDefinition(recorder, "stop-consumer", &dependency, consumer),
	)

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)

	require.Error(t, completed.err)
	assert.Equal(t, 1, completed.code)
	assert.Contains(t, completed.err.Error(), "consumer stop exploded",
		"a panicking Stop is recovered and reported, not propagated")
	assert.Contains(t, completed.err.Error(), "provider stop failed",
		"the panic must not truncate the unwind before the next plugin's Stop")
	assert.Subset(t, recorder.snapshot(), []string{"stop:stop-consumer", "stop:stop-provider"})
}

// TestConcurrentSignalAndCriticalFailureUnwindExactlyOnce pins that two
// independent shutdown triggers racing each other still produce one unwind:
// every Stop runs exactly once, and one reason wins.
func TestConcurrentSignalAndCriticalFailureUnwindExactlyOnce(t *testing.T) {
	recorder := &readinessRecorder{}
	var app *App
	racing := readinessStages(recorder, "racing")
	racing.Start = func(_ *readinessValue, ctx *plugin.Context) error {
		recorder.record("start:racing")
		var starter sync.WaitGroup
		starter.Add(2)
		go func() { defer starter.Done(); app.requestStop(stopReasonSignal) }()
		require.True(t, ctx.GoCritical(func(context.Context) {
			defer starter.Done()
			panic(errors.New("critical exploded"))
		}))
		starter.Wait()
		return nil
	}
	app = newRuntimeTestApp(readinessDefinition(recorder, "racing", nil, racing))

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	completed := awaitRuntimeTestResult(t, result)

	stops := 0
	for _, event := range recorder.snapshot() {
		if event == "stop:racing" {
			stops++
		}
	}
	assert.Equal(t, 1, stops, "two concurrent shutdown triggers must not unwind twice")
	assert.NotEmpty(t, app.currentStopReason(), "exactly one reason must have been recorded")
	if completed.err != nil {
		assert.Equal(t, 1, completed.code)
	}
}
