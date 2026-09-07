package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
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

	// Either trigger may win the reason, but neither outcome may swallow the
	// panic: it is recorded against its owning plugin and surfaces through the
	// unwind regardless. Tolerating a nil error here would let a run that lost
	// the critical failure pass.
	assert.Contains(t, []string{stopReasonSignal, stopReasonCritical}, app.currentStopReason(),
		"exactly one of the two racing triggers must own the shutdown")
	require.Error(t, completed.err, "the panicking critical task must be reported whichever trigger won")
	assert.Equal(t, 1, completed.code)
	assert.Contains(t, completed.err.Error(), "critical exploded")
}

// TestShutdownIsBoundedWhenStopNeverReturnsAndSkipsTheDependency is the
// inverted port of the pre-migration TestShutdownBoundedWhenStopNeverReturns.
//
// The original asserted that the dependency's Stop is still *called* after the
// hanging consumer burned the shared budget. The §6.2 ruling reverses that
// clause: after the budget is spent no new Stop is started, and the remaining
// identities are reported as not-attempted. Everything else the original
// pinned is kept — the whole run stays bounded, and the diagnostic names the
// stalled plugin rather than reporting one anonymous shutdown failure.
//
// `stopCalls` is the discriminating assertion. Reverting to the old behaviour
// would raise it to one and empty NotAttempted; a report-only check would not
// notice the dependency's Stop body running concurrently with the abandoned
// one, which is exactly what made reverse order unobservable before.
func TestShutdownIsBoundedWhenStopNeverReturnsAndSkipsTheDependency(t *testing.T) {
	recorder := &readinessRecorder{}
	dependency := plugin.RefTo[readinessContract]("dependency")

	var stopCalls atomic.Int32
	provider := readinessStages(recorder, "dependency")
	provider.Stop = func(*readinessValue, context.Context) error {
		stopCalls.Add(1)
		recorder.record("stop:dependency")
		return nil
	}
	hangEntered := make(chan struct{})
	releaseHang := make(chan struct{})
	defer close(releaseHang)
	hanger := readinessStages(recorder, "hanger")
	hanger.Stop = func(*readinessValue, context.Context) error {
		recorder.record("stop:hanger")
		close(hangEntered)
		// Ignore the deadline context entirely: this models the plugin
		// contract violation the runtime, not the plugin, has to survive.
		<-releaseHang
		return nil
	}
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "dependency", nil, provider),
		readinessDefinition(recorder, "hanger", &dependency, hanger),
	)

	result := executeRuntimeTest(app, runtimeTestConfig(t, 200*time.Millisecond)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	select {
	case <-hangEntered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the reverse walk never reached the hanging consumer")
	}
	// awaitRuntimeTestResult's own bound is what turns "the run wedged on a
	// Stop that never returns" into a reported failure instead of a hung
	// test binary.
	completed := awaitRuntimeTestResult(t, result)

	assert.Equal(t, 1, completed.code, "a Stop that outlives the budget must not exit zero")
	require.Error(t, completed.err)
	assert.Contains(t, completed.err.Error(), "xbc: plugin hanger Stop did not return within shutdown budget",
		"the diagnostic must name the stalled plugin")
	assert.Contains(t, completed.err.Error(), "xbc: plugin dependency Stop was not attempted",
		"the skipped cleanup must be named, not silently dropped")

	assert.Equal(t, []plugin.Identity{{Plugin: "hanger", Instance: plugin.DefaultInstance}},
		app.shutdownReport.Identities(assembly.StopAbandoned))
	assert.Equal(t, []plugin.Identity{{Plugin: "dependency", Instance: plugin.DefaultInstance}},
		app.shutdownReport.Identities(assembly.StopNotAttempted))
	assert.Zero(t, stopCalls.Load(),
		"no Stop may start after the shared budget is spent, so the dependency is never entered")
	assert.NotContains(t, recorder.snapshot(), "stop:dependency")
}

// TestShutdownIsStrictlySerialInReverseDependencyOrder pins that the normal
// path completes each Stop before the next one begins. The `inside` counter
// fails the moment two Stop bodies overlap, which is what an implementation
// that launched every Stop hook concurrently would produce; asserting only the
// final order would let such an implementation pass whenever the scheduler
// happened to run the goroutines in the right sequence.
func TestShutdownIsStrictlySerialInReverseDependencyOrder(t *testing.T) {
	recorder := &readinessRecorder{}
	var (
		mu      sync.Mutex
		inside  int
		overlap bool
	)
	stages := func(key plugin.Key) plugin.Lifecycle[*readinessValue] {
		lifecycle := readinessStages(recorder, key)
		lifecycle.Stop = func(*readinessValue, context.Context) error {
			mu.Lock()
			inside++
			overlap = overlap || inside > 1
			mu.Unlock()
			recorder.record("stop:" + string(key))
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			return nil
		}
		return lifecycle
	}
	first := plugin.RefTo[readinessContract]("serial-first")
	second := plugin.RefTo[readinessContract]("serial-second")
	app := newRuntimeTestApp(
		readinessDefinition(recorder, "serial-first", nil, stages("serial-first")),
		readinessDefinition(recorder, "serial-second", &first, stages("serial-second")),
		readinessDefinition(recorder, "serial-third", &second, stages("serial-third")),
	)

	result := executeRuntimeTest(app, runtimeTestConfig(t, 5*time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)

	events := recorder.snapshot()
	assert.Equal(t, []string{"stop:serial-third", "stop:serial-second", "stop:serial-first"},
		events[len(events)-3:])
	want := []plugin.Identity{
		{Plugin: "serial-third", Instance: plugin.DefaultInstance},
		{Plugin: "serial-second", Instance: plugin.DefaultInstance},
		{Plugin: "serial-first", Instance: plugin.DefaultInstance},
	}
	assert.Equal(t, want, app.shutdownReport.Attempted)
	assert.Equal(t, want, app.shutdownReport.Completed,
		"completion order must match invocation order on the normal path")
	mu.Lock()
	defer mu.Unlock()
	assert.False(t, overlap, "two Stop bodies must never run at the same time")
}
