package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/plugin"
)

const runtimeTestTimeout = 5 * time.Second

type runtimeTestResult struct {
	code int
	err  error
}

type runtimeTestValue struct{}

func newRuntimeTestApp(definitions ...plugin.Definition) *App {
	app := newApp([]plugin.Bundle{plugin.BundleOf(definitions...)})
	app.ready = make(chan struct{})
	return app
}

func runtimeTestConfig(t *testing.T, shutdownTimeout time.Duration) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := []byte("log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: " + shutdownTimeout.String() + "\n")
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return []string{"--config", path}
}

func executeRuntimeTest(app *App, args ...string) <-chan runtimeTestResult {
	result := make(chan runtimeTestResult, 1)
	go func() {
		code, err := app.Execute(context.Background(), args)
		result <- runtimeTestResult{code: code, err: err}
	}()
	return result
}

func awaitRuntimeTestReady(t *testing.T, app *App) {
	t.Helper()
	select {
	case <-app.ready:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("application did not reach the running state")
	}
}

func awaitRuntimeTestResult(t *testing.T, result <-chan runtimeTestResult) runtimeTestResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(runtimeTestTimeout):
		t.Fatal("application did not finish")
		return runtimeTestResult{}
	}
}

func assertChannelOpen(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal(message)
	default:
	}
}

func TestDoctorBuildsPlanWithoutConstructing(t *testing.T) {
	var factories atomic.Int32
	definition := plugin.Define("doctor-plan-only", func(plugin.BuildContext) (*runtimeTestValue, error) {
		factories.Add(1)
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)

	args := append([]string{"doctor"}, runtimeTestConfig(t, time.Second)...)
	code, err := app.Execute(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Zero(t, factories.Load(), "doctor must stop after planning")
	require.NotNil(t, app.plan)
	assert.Nil(t, app.owned)
}

func TestManagedTasksAreAdmittedOnlyDuringOwningStart(t *testing.T) {
	var (
		initAccepted    bool
		startAccepted   bool
		trafficAccepted bool
		retained        *plugin.Context
	)
	taskStarted := make(chan struct{})
	taskStopped := make(chan struct{})
	definition := plugin.Define("task-admission", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Init: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			initAccepted = ctx.Go(func(context.Context) {})
			return nil
		},
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			retained = ctx
			startAccepted = ctx.Go(func(taskContext context.Context) {
				close(taskStarted)
				<-taskContext.Done()
				close(taskStopped)
			})
			return nil
		},
		OpenTraffic: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			trafficAccepted = ctx.Go(func(context.Context) {})
			return nil
		},
	}})
	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)

	select {
	case <-taskStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("Start-owned managed task did not run")
	}
	assert.False(t, initAccepted)
	assert.True(t, startAccepted)
	assert.False(t, trafficAccepted)
	require.NotNil(t, retained)
	assert.False(t, retained.Go(func(context.Context) {}), "admission must close before Start returns")

	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	select {
	case <-taskStopped:
	default:
		t.Fatal("Execute returned before the managed task joined")
	}
}

func TestUnwindStopsBeforeCancelingAndJoiningManagedTasks(t *testing.T) {
	var (
		eventsMu sync.Mutex
		events   []string
	)
	record := func(event string) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	}
	taskStarted := make(chan struct{})
	taskCanceled := make(chan struct{})
	definition := plugin.Define("stop-cancel-join", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			require.True(t, ctx.Go(func(taskContext context.Context) {
				close(taskStarted)
				<-taskContext.Done()
				record("cancel")
				close(taskCanceled)
			}))
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		Stop: func(*runtimeTestValue, context.Context) error {
			select {
			case <-taskCanceled:
				return errors.New("managed task was canceled before Stop")
			default:
			}
			record("stop")
			return nil
		},
	}})
	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)
	select {
	case <-taskStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("managed task did not start")
	}

	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	eventsMu.Lock()
	assert.Equal(t, []string{"stop", "cancel"}, events)
	eventsMu.Unlock()
}

func TestLifecycleFailuresUnwindOwnedValuesAndKeepGateClosed(t *testing.T) {
	stageFailure := errors.New("stage failed")
	tests := []struct {
		name      string
		configure func(*plugin.Lifecycle[*runtimeTestValue])
		prefix    []string
		stage     string
	}{
		{
			name: "migration",
			configure: func(lifecycle *plugin.Lifecycle[*runtimeTestValue]) {
				lifecycle.Migrate = func(*runtimeTestValue, *plugin.Context) error { return stageFailure }
			},
			prefix: []string{"--migrate"},
			stage:  "Migrate",
		},
		{
			name: "start",
			configure: func(lifecycle *plugin.Lifecycle[*runtimeTestValue]) {
				lifecycle.Start = func(*runtimeTestValue, *plugin.Context) error { return stageFailure }
			},
			stage: "Start",
		},
		{
			name: "traffic preparation",
			configure: func(lifecycle *plugin.Lifecycle[*runtimeTestValue]) {
				lifecycle.OpenTraffic = func(*runtimeTestValue, *plugin.Context) error { return stageFailure }
			},
			stage: "OpenTraffic",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stops atomic.Int32
			lifecycle := plugin.Lifecycle[*runtimeTestValue]{
				Stop: func(*runtimeTestValue, context.Context) error {
					stops.Add(1)
					return nil
				},
			}
			test.configure(&lifecycle)
			definition := plugin.Define(plugin.Key("failure-"+strings.ReplaceAll(test.name, " ", "-")), func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}, plugin.Options[*runtimeTestValue]{Lifecycle: lifecycle})
			app := newRuntimeTestApp(definition)
			args := append(append([]string(nil), test.prefix...), runtimeTestConfig(t, time.Second)...)

			code, err := app.Execute(context.Background(), args)
			assert.Equal(t, 1, code)
			require.Error(t, err)
			assert.ErrorIs(t, err, stageFailure)
			assert.Contains(t, err.Error(), test.stage)
			assert.Equal(t, int32(1), stops.Load())
			assertChannelOpen(t, app.trafficGate, "traffic gate opened after lifecycle failure")
		})
	}
}

func TestTrafficTasksCannotExposeIngressBeforeGlobalRelease(t *testing.T) {
	preparing := make(chan struct{})
	finishPreparation := make(chan struct{})
	ingressVisible := make(chan struct{})
	definition := plugin.Define("traffic-barrier", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			gate := ctx.TrafficGate()
			require.True(t, ctx.Go(func(taskContext context.Context) {
				select {
				case <-gate:
					close(ingressVisible)
				case <-taskContext.Done():
				}
			}))
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error {
			close(preparing)
			<-finishPreparation
			return nil
		},
	}})
	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	select {
	case <-preparing:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("traffic preparation did not begin")
	}
	assertChannelOpen(t, ingressVisible, "ingress became visible during fallible preparation")
	close(finishPreparation)
	awaitRuntimeTestReady(t, app)
	select {
	case <-ingressVisible:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("ingress did not become visible after global gate release")
	}

	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
}

func TestCriticalTaskDuringLaterStartCannotLoseRaceToTrafficRelease(t *testing.T) {
	releaseCritical := make(chan struct{})
	laterStartEntered := make(chan struct{})
	finishLaterStart := make(chan struct{})
	critical := plugin.Define("critical-a", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			require.True(t, ctx.GoCritical(func(context.Context) { <-releaseCritical }))
			return nil
		},
	}})
	later := plugin.Define("later-b", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error {
			close(laterStartEntered)
			<-finishLaterStart
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	app := newRuntimeTestApp(critical, later)
	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	select {
	case <-laterStartEntered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("later Start did not begin")
	}
	close(releaseCritical)
	select {
	case <-app.stopCh:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("critical task return did not request shutdown")
	}
	close(finishLaterStart)

	completed := awaitRuntimeTestResult(t, result)
	assert.Equal(t, 1, completed.code)
	require.Error(t, completed.err)
	assert.Contains(t, completed.err.Error(), "received stop request during startup")
	assertChannelOpen(t, app.trafficGate, "traffic gate opened after a critical shutdown request")
}

func TestLifecyclePanicAndStopPanicAreAggregated(t *testing.T) {
	definition := plugin.Define("panic-aggregation", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error { panic("start exploded") },
		Stop:  func(*runtimeTestValue, context.Context) error { panic("stop exploded") },
	}})
	app := newRuntimeTestApp(definition)
	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Start panic: start exploded")
	assert.Contains(t, err.Error(), "Stop panic: stop exploded")
}

// TestShutdownBudgetAbandonsTheStuckStopAndAttemptsNoFurtherStop replaces the
// earlier "budget does not truncate reverse unwind" contract. That contract
// required the walk to keep calling Stop after the shared budget was spent,
// which left the abandoned Stop running concurrently with the next one and
// made the recorded order a scheduling outcome rather than a guarantee. The
// ruling is now the opposite: once the budget is spent, no new Stop starts.
//
// Every assertion below fails if that regresses. `stopped` would grow past the
// stuck plugin; report.Attempted would name more than the stuck plugin; and
// NotAttempted would come back empty.
func TestShutdownBudgetAbandonsTheStuckStopAndAttemptsNoFurtherStop(t *testing.T) {
	var (
		mu      sync.Mutex
		stopped []string
	)
	record := func(key string) {
		mu.Lock()
		stopped = append(stopped, key)
		mu.Unlock()
	}
	stuckEntered := make(chan struct{})
	releaseStuck := make(chan struct{})
	makeDefinition := func(key plugin.Key, stop func()) plugin.Definition {
		lifecycle := plugin.Lifecycle[*runtimeTestValue]{
			Stop: func(*runtimeTestValue, context.Context) error {
				stop()
				return nil
			},
		}
		if key == "a-live" {
			lifecycle.OpenTraffic = func(*runtimeTestValue, *plugin.Context) error { return nil }
		}
		return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		}, plugin.Options[*runtimeTestValue]{Lifecycle: lifecycle})
	}
	app := newRuntimeTestApp(
		makeDefinition("a-live", func() { record("a-live") }),
		makeDefinition("b-next", func() { record("b-next") }),
		makeDefinition("c-stuck", func() {
			record("c-stuck")
			close(stuckEntered)
			<-releaseStuck
		}),
	)
	result := executeRuntimeTest(app, runtimeTestConfig(t, 200*time.Millisecond)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	select {
	case <-stuckEntered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("reverse unwind did not start with the last plugin")
	}
	completed := awaitRuntimeTestResult(t, result)
	close(releaseStuck)

	assert.Equal(t, 1, completed.code)
	require.Error(t, completed.err)
	assert.Contains(t, completed.err.Error(), "xbc: plugin c-stuck Stop did not return within shutdown budget")
	assert.Contains(t, completed.err.Error(), "xbc: plugin b-next Stop was not attempted")
	assert.Contains(t, completed.err.Error(), "xbc: plugin a-live Stop was not attempted")

	stuck := plugin.Identity{Plugin: "c-stuck", Instance: plugin.DefaultInstance}
	assert.Equal(t, []plugin.Identity{stuck}, app.shutdownReport.Attempted)
	assert.Equal(t, []plugin.Identity{stuck}, app.shutdownReport.Identities(assembly.StopAbandoned))
	assert.Equal(t, []plugin.Identity{
		{Plugin: "b-next", Instance: plugin.DefaultInstance},
		{Plugin: "a-live", Instance: plugin.DefaultInstance},
	}, app.shutdownReport.Identities(assembly.StopNotAttempted))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"c-stuck"}, stopped,
		"no Stop body below the stuck plugin may be entered after the budget is spent")
}

// TestShutdownCancelsTaskScopesTheSpentBudgetNeverReached closes the leak
// hole opened by the "start no further Stop" ruling: a plugin whose Stop is
// never attempted also never gets its afterStop, so its managed task scope can
// only be reclaimed by the drain that follows the walk.
//
// The assertion discriminates because the task blocks on its own context and
// nothing else ever cancels it: if the drain stopped reclaiming scopes the walk
// did not reach, taskCanceled would never close and the goroutine would outlive
// the process shutdown.
func TestShutdownCancelsTaskScopesTheSpentBudgetNeverReached(t *testing.T) {
	taskStarted := make(chan struct{})
	taskCanceled := make(chan struct{})
	stuckEntered := make(chan struct{})
	releaseStuck := make(chan struct{})
	defer close(releaseStuck)

	leaky := plugin.Define("leaky-first", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			require.True(t, ctx.Go(func(taskContext context.Context) {
				close(taskStarted)
				<-taskContext.Done()
				close(taskCanceled)
			}))
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	stuck := plugin.Define("z-stuck", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Stop: func(*runtimeTestValue, context.Context) error {
			close(stuckEntered)
			<-releaseStuck
			return nil
		},
	}})

	app := newRuntimeTestApp(leaky, stuck)
	result := executeRuntimeTest(app, runtimeTestConfig(t, 200*time.Millisecond)...)
	awaitRuntimeTestReady(t, app)
	select {
	case <-taskStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the managed task never started")
	}

	app.requestStop(stopReasonSignal)
	select {
	case <-stuckEntered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the reverse unwind never reached the stuck plugin")
	}
	completed := awaitRuntimeTestResult(t, result)

	assert.Equal(t, 1, completed.code)
	require.Error(t, completed.err)
	assert.Equal(t, []plugin.Identity{{Plugin: "leaky-first", Instance: plugin.DefaultInstance}},
		app.shutdownReport.Identities(assembly.StopNotAttempted),
		"the plugin owning the task is the one the spent budget skipped")

	select {
	case <-taskCanceled:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("a task scope the reverse walk never reached was left running")
	}
}
