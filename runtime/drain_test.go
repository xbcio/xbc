package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// drainObservation is what one Drain hook saw, recorded so the assertions can
// run on the test goroutine after the application has finished. The hook runs
// on its own goroutine, so this is the only safe way to read it.
type drainObservation struct {
	mu    sync.Mutex
	calls []string
}

func (o *drainObservation) record(call string) {
	o.mu.Lock()
	o.calls = append(o.calls, call)
	o.mu.Unlock()
}

func (o *drainObservation) recordedCalls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

// ingressLiveDefinition builds a Definition that opens traffic (a
// TrafficOpener, standing in for web.Server) and optionally stops, so a test
// composition has a real ingress closure for Unwind to stop in phase A.
func ingressLiveDefinition(key plugin.Key, onStop func()) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		Stop: func(*runtimeTestValue, context.Context) error {
			if onStop != nil {
				onStop()
			}
			return nil
		},
	}})
}

// drainLiveDefinition builds a non-ingress Definition that declares a Drain
// hook, for a composition that already carries an ingressLiveDefinition.
func drainLiveDefinition(key plugin.Key, observation *drainObservation, drainFn func(context.Context) error) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Drain: func(_ *runtimeTestValue, ctx context.Context) error {
			observation.record(key.String())
			if drainFn != nil {
				return drainFn(ctx)
			}
			return nil
		},
	}})
}

// runtimeDrainConfig writes the framework section with every relevant knob
// spelled out, because every assertion here is about which budget bounded
// what.
func runtimeDrainConfig(t *testing.T, drain, shutdown time.Duration) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: " + shutdown.String() + "\n  drain_timeout: " + drain.String() + "\n  pre_stop_timeout: 0s\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return []string{"--config", path}
}

// TestDrainRunsAfterIngressStopAndBeforeRemainingStops pins the phase's
// position in the real shutdown sequence end to end: the ingress plugin's
// Stop must have already run by the time Drain is called on a
// non-ingress plugin, which is the whole point of the phase existing between
// them.
func TestDrainRunsAfterIngressStopAndBeforeRemainingStops(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	order := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}

	ingress := ingressLiveDefinition("ingress", func() { record("stop ingress") })
	drainer := plugin.Define("drainer", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Drain: func(*runtimeTestValue, context.Context) error { record("drain drainer"); return nil },
		Stop:  func(*runtimeTestValue, context.Context) error { record("stop drainer"); return nil },
	}})

	app := newRuntimeTestApp(ingress, drainer)
	result := executeRuntimeTest(app, runtimeDrainConfig(t, time.Second, 5*time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)

	recorded := order()
	require.Equal(t, []string{"stop ingress", "drain drainer", "stop drainer"}, recorded,
		"the ingress closure is stopped first, then the drain phase runs, then the remaining Stop")
}

// TestDrainBudgetStartsWhenThePhaseBeginsEndToEnd pins at the runtime level
// that drain_timeout is the drain phase's own budget: an ingress Stop in
// phase A that outlasts drain_timeout must not spend it, so the Drain hook
// still runs with close to its full budget, bounded only by the walk's
// shutdown deadline. Before the phase deadline was derived inside phase B,
// the slow ingress Stop consumed the entire drain budget and this drainer was
// reported not-attempted without ever being called.
func TestDrainBudgetStartsWhenThePhaseBeginsEndToEnd(t *testing.T) {
	const (
		drainTimeout    = 300 * time.Millisecond
		ingressStopCost = 2 * drainTimeout
	)
	observation := &drainObservation{}
	remaining := make(chan time.Duration, 1)
	ingress := ingressLiveDefinition("ingress-slow-stop", func() { time.Sleep(ingressStopCost) })
	drainer := drainLiveDefinition("drainer-live-budget", observation, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			remaining <- -1
			return nil
		}
		remaining <- time.Until(deadline)
		return nil
	})

	app := newRuntimeTestApp(ingress, drainer)
	result := executeRuntimeTest(app, runtimeDrainConfig(t, drainTimeout, 2*time.Second)...)
	awaitRuntimeTestReady(t, app)
	require.True(t, app.requestStop(stopReasonSignal))
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	require.Equal(t, 0, completed.code)

	select {
	case left := <-remaining:
		assert.Greater(t, left, drainTimeout/2,
			"the drain phase's budget must start when the phase begins: the slow ingress Stop must not spend it")
	default:
		t.Fatal("Drain was never called: the slow ingress Stop consumed the drain budget")
	}
}

// TestDrainTimeoutZeroSkipsThePhaseEndToEnd pins the documented off switch at
// the runtime level: the hook must not be called at all when drain_timeout is
// 0s, exactly as pre_stop_timeout's off switch is pinned by
// TestPreStopBudgetOfZeroSkipsThePhase.
func TestDrainTimeoutZeroSkipsThePhaseEndToEnd(t *testing.T) {
	observation := &drainObservation{}
	ingress := ingressLiveDefinition("ingress-off", nil)
	drainer := drainLiveDefinition("drainer-off", observation, nil)

	app := newRuntimeTestApp(ingress, drainer)
	result := executeRuntimeTest(app, runtimeDrainConfig(t, 0, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	assert.Empty(t, observation.recordedCalls(), "drain_timeout 0s must not call the hook")
}

// TestDrainAbortPathAlsoDrains proves the abort (startup failure) path goes
// through the same drain logic as an ordinary shutdown: a plugin that fails
// Start still leaves every already-started Drainer drained during the unwind
// abort triggers.
//
// It uses two plugins ordered so the second (which fails Start) depends on
// nothing from the first; the first reaches Start and is therefore eligible
// for Drain during the abort unwind that follows.
func TestDrainAbortPathAlsoDrains(t *testing.T) {
	observation := &drainObservation{}
	drainer := drainLiveDefinition("drainer-abort", observation, nil)
	failing := plugin.Define("start-fails", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error { return assert.AnError },
	}})

	app := newRuntimeTestApp(drainer, failing)
	result := executeRuntimeTest(app, runtimeDrainConfig(t, time.Second, 5*time.Second)...)
	completed := awaitRuntimeTestResult(t, result)
	require.Error(t, completed.err)

	assert.Equal(t, []string{"drainer-abort"}, observation.recordedCalls(),
		"the abort path reaches unwind, which must drain an already-started Drainer the same way an ordinary shutdown would")
}

// TestDrainReportReachesTheShutdownReport pins that the phase is visible to an
// operator reading the stop's log, exactly as TestPreStopReportReachesTheShutdownReport
// pins the pre-stop phase's visibility.
func TestDrainReportReachesTheShutdownReport(t *testing.T) {
	observation := &drainObservation{}
	ingress := ingressLiveDefinition("ingress-reported", nil)
	drainer := drainLiveDefinition("drainer-reported", observation, nil)

	app := newRuntimeTestApp(ingress, drainer)
	capture, code, err := stopUnderDrainCapture(t, app, 500*time.Millisecond, time.Second)
	require.NoError(t, err)
	require.Equal(t, 0, code)

	entry := preStopEntryFor(t, capture, "xbc: drain phase finished inside its budget")
	assert.Equal(t, "debug", entry.level, "a clean phase is not a warning")
	fields := entry.fields()
	assert.Equal(t, "500ms", fields["budget"])
	waited, ok := fields["waited"].([]string)
	require.True(t, ok)
	require.Len(t, waited, 1)
	assert.Contains(t, waited[0], "drainer-reported ")

	assert.Empty(t, preStopEntriesFor(capture, "xbc: drain hooks did not finish cleanly"))
	assert.Empty(t, preStopEntriesFor(capture, "xbc: drain budget expired before every hook returned"))
}

// TestDrainBudgetExpiryIsReportedWithTheAbandonedPlugins pins the one signal
// an operator gets that a Drain hook ignored the phase budget, mirroring
// TestPreStopBudgetExpiryIsReportedWithTheAbandonedPlugins for the drain
// phase.
func TestDrainBudgetExpiryIsReportedWithTheAbandonedPlugins(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	observation := &drainObservation{}
	ingress := ingressLiveDefinition("ingress-stuck-drain", nil)
	stuck := plugin.Define("stuck-drain", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Drain: func(*runtimeTestValue, context.Context) error {
			observation.record("stuck-drain")
			<-release
			return nil
		},
	}})

	app := newRuntimeTestApp(ingress, stuck)
	capture, code, err := stopUnderDrainCapture(t, app, 50*time.Millisecond, time.Second)
	require.NoError(t, err, "an abandoned Drain is reported, not turned into a failed run, exactly like an abandoned PreStop")
	require.Equal(t, 0, code)

	entry := preStopEntryFor(t, capture, "xbc: drain budget expired before every hook returned")
	assert.Equal(t, "warn", entry.level)
	fields := entry.fields()
	assert.Equal(t, (50 * time.Millisecond).String(), fields["budget"])
	assert.ElementsMatch(t, []string{"stuck-drain"}, fields["abandoned"])
	assert.Empty(t, fields["waited"])
}

// stopUnderDrainCapture is stopUnderCapture's drain-phase counterpart: it
// drives one complete stop on the calling goroutine with a recording logger,
// with the drain and shutdown budgets spelled out in the section bootstrap
// decodes, exactly as stopUnderCapture spells out the pre-stop budget.
func stopUnderDrainCapture(t *testing.T, app *App, drainTimeout, shutdownTimeout time.Duration) (*captureLogger, int, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: " + shutdownTimeout.String() + "\n  drain_timeout: " + drainTimeout.String() + "\n  pre_stop_timeout: 0s\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	cmd, err := parseArgs([]string{"doctor", "--config", path}, config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	plan, planErr := assembly.BuildPlan(assembly.PlanOptions{Bundles: app.bundles, Env: app.env, Logger: app.logger})
	require.NoError(t, planErr)
	capture := &captureLogger{}
	app.logger = capture

	owned, constructErr := assembly.Construct(plan, assembly.ConstructOptions{ShutdownTimeout: app.settings.ShutdownTimeout})
	require.NoError(t, constructErr)
	app.owned = owned

	require.True(t, app.releaseTraffic(), "the drain phase runs only once ingress has been opened")
	require.True(t, app.requestStop(stopReasonSignal))
	code, waitErr := app.wait()
	return capture, code, waitErr
}
