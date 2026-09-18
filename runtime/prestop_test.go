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

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// preStopObservation is what one PreStop hook saw, recorded so the assertions
// can run on the test goroutine after the application has finished. The hook
// runs on its own goroutine, so this is the only safe way to read it.
type preStopObservation struct {
	mu sync.Mutex

	calls             []string
	contextErrs       []error
	deadlines         []time.Duration
	contextsCancelled []bool
}

// record captures the two facts every test in this file turns on: what the
// hook's own context looked like, and what the application's execution context
// looked like at the same instant.
func (o *preStopObservation) record(call string, hookCtx, executionCtx context.Context) {
	remaining := time.Duration(0)
	if deadline, ok := hookCtx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	cancelled := executionCtx == nil || executionCtx.Err() != nil
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	o.contextErrs = append(o.contextErrs, hookCtx.Err())
	o.deadlines = append(o.deadlines, remaining)
	o.contextsCancelled = append(o.contextsCancelled, cancelled)
}

func (o *preStopObservation) recordedCalls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *preStopObservation) last() (err error, remaining time.Duration, executionCancelled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.calls) == 0 {
		return nil, 0, false
	}
	index := len(o.calls) - 1
	return o.contextErrs[index], o.deadlines[index], o.contextsCancelled[index]
}

// executionContextOf reads the application's execution context the way
// hostAdapter exposes it to a plugin, under the mutex that guards it.
func executionContextOf(app *App) context.Context {
	app.stateMu.Lock()
	defer app.stateMu.Unlock()
	return app.executionCtx
}

// preStopLiveDefinition builds a Definition that carries the lifecycle under
// test and also opens traffic, so the application reaches the running state
// the phase requires. assertLiveness refuses a process with neither traffic nor
// a critical managed task, and the phase itself runs only once the traffic gate
// has been released, so a test definition without one of the two never gets as
// far as stopping.
func preStopLiveDefinition(key plugin.Key, lifecycle plugin.Lifecycle[*runtimeTestValue]) plugin.Definition {
	lifecycle.OpenTraffic = func(*runtimeTestValue, *plugin.Context) error { return nil }
	return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: lifecycle})
}

// runtimePreStopConfig writes the framework section with both stop budgets
// spelled out, because every assertion here is about which of the two bounded
// what.
func runtimePreStopConfig(t *testing.T, preStop, shutdown time.Duration) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: " + shutdown.String() + "\n  pre_stop_timeout: " + preStop.String() + "\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return []string{"--config", path}
}

// stopUnderCapture drives one complete stop on the calling goroutine with a
// recording logger in place of the real one, and reports the exit code the
// process would have used.
//
// The log assertions need a different seam from the ones above, and the reason
// is structural rather than stylistic: bootstrap installs the real logger on
// every path that runs an application, so a test that drives Execute end to end
// cannot read the lines its own shutdown produced. ownUnderCapture replaces that
// logger with a recorder, and the two calls below put the application in the
// state a running one is in when the stop arrives -- the traffic gate open, then
// a stop requested -- and then reuse wait, which is the same function that turns
// the unwind's outcome into an exit code. So the assertions run against the real
// shutdown sequence rather than a reimplementation of it; only the logger and the
// process tail differ.
//
// It asserts that the gate opens rather than assuming it: a phase that was
// silently skipped because trafficOpen was never set would otherwise look
// identical to one that ran and said nothing, which is the exact confusion
// reportPreStop's silence for a skipped phase depends on being distinguishable.
func stopUnderCapture(t *testing.T, app *App, preStopTimeout time.Duration) (*captureLogger, int, error) {
	t.Helper()
	// This mirrors ownUnderCapture with one difference: the phase budget has to
	// be part of the section bootstrap decodes, so it is passed as the extra
	// fragment planUnderCapture appends to "xbc:" rather than assigned to
	// app.settings afterwards -- a budget that never went through binding would
	// not be the budget the phase reads.
	plan, capture := planUnderCapture(t, app, "  pre_stop_timeout: "+preStopTimeout.String()+"\n")
	owned, err := assembly.Construct(plan, assembly.ConstructOptions{ShutdownTimeout: app.settings.ShutdownTimeout})
	require.NoError(t, err)
	app.owned = owned

	require.True(t, app.releaseTraffic(), "the pre-stop phase runs only once ingress has been opened")
	require.True(t, app.requestStop(stopReasonSignal))
	code, err := app.wait()
	return capture, code, err
}

// preStopEntryFor returns the single captured entry carrying message. Selecting
// by message rather than by position is what keeps an assertion about one line
// from depending on how many other lines the same stop happened to emit, which
// is also what makes the silence half of these tests expressible: a line that
// must not appear is asserted by requiring no entry carries it.
func preStopEntryFor(t *testing.T, capture *captureLogger, message string) captureEntry {
	t.Helper()
	var found []captureEntry
	for _, entry := range capture.entries {
		if entry.msg == message {
			found = append(found, entry)
		}
	}
	require.Len(t, found, 1, "exactly one %q record was expected", message)
	return found[0]
}

// preStopEntriesFor is preStopEntryFor for the cases that assert a line is
// absent: it returns every record carrying message, so "none" and "one" are
// different answers a caller can state.
func preStopEntriesFor(capture *captureLogger, message string) []captureEntry {
	var found []captureEntry
	for _, entry := range capture.entries {
		if entry.msg == message {
			found = append(found, entry)
		}
	}
	return found
}

// TestPreStopReceivesALiveContextWhileTheExecutionContextIsCancelled is the
// single most important assertion about this stage.
//
// PreStop exists to make remote calls -- releasing a lease, withdrawing an
// address -- while the process is still alive, and by the time it runs the
// stop request has already cancelled the application's execution context. If
// the hook were handed that context, or the *plugin.Context that delegates
// Done and Err to it, every call would fail instantly with context.Canceled.
// Because a failed PreStop is only logged, that failure would be silent and
// total: the shutdown would look clean while nothing had been released.
//
// The test asserts both halves at one instant: the hook's context is alive
// with a real deadline, and the execution context is already dead beside it.
func TestPreStopReceivesALiveContextWhileTheExecutionContextIsCancelled(t *testing.T) {
	observation := &preStopObservation{}
	const budget = 3 * time.Second

	var app *App
	definition := preStopLiveDefinition("prestop-context", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(_ *runtimeTestValue, ctx context.Context) error {
			observation.record("prestop", ctx, executionContextOf(app))
			return nil
		},
	})
	app = newRuntimeTestApp(definition)

	result := executeRuntimeTest(app, runtimePreStopConfig(t, budget, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)

	require.Equal(t, []string{"prestop"}, observation.recordedCalls(),
		"the phase must run on the ordinary signal-driven shutdown path")
	hookErr, remaining, executionCancelled := observation.last()
	assert.NoError(t, hookErr,
		"the hook's context must be alive: a cancelled one turns every release into a silent failure")
	assert.True(t, executionCancelled,
		"the execution context is already cancelled when the phase runs, which is why the hook must not be given it")
	assert.Greater(t, remaining, time.Duration(0), "the hook's context carries the pre-stop budget")
	assert.LessOrEqual(t, remaining, budget+time.Second)
}

// TestPreStopRunsBeforeAnyStopDoes pins the phase's position in the shutdown
// sequence. It is the whole point of the stage: a value that is still visible
// to its peers has to become invisible before anything starts tearing down.
func TestPreStopRunsBeforeAnyStopDoes(t *testing.T) {
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

	first := preStopLiveDefinition("a-first", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error { record("prestop a-first"); return nil },
		Stop:    func(*runtimeTestValue, context.Context) error { record("stop a-first"); return nil },
	})
	second := preStopLiveDefinition("b-second", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error { record("prestop b-second"); return nil },
		Stop:    func(*runtimeTestValue, context.Context) error { record("stop b-second"); return nil },
	})

	app := newRuntimeTestApp(first, second)
	result := executeRuntimeTest(app, runtimePreStopConfig(t, time.Second, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)

	recorded := order()
	require.Len(t, recorded, 4)
	assert.ElementsMatch(t, []string{"prestop a-first", "prestop b-second"}, recorded[:2],
		"every PreStop is initiated before the first Stop begins")
	assert.ElementsMatch(t, []string{"stop a-first", "stop b-second"}, recorded[2:],
		"the reverse walk still owns the Stop order, and PreStop does not disturb it")
}

// TestPreStopBudgetOfZeroSkipsThePhase pins the documented off switch. The
// hook must not be called at all: "skipped" and "called and did nothing" are
// different contracts, and only the first one lets an application opt out of
// the stage's cost entirely.
func TestPreStopBudgetOfZeroSkipsThePhase(t *testing.T) {
	observation := &preStopObservation{}
	definition := preStopLiveDefinition("prestop-off", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error {
			observation.record("prestop", context.Background(), context.Background())
			return nil
		},
	})
	app := newRuntimeTestApp(definition)

	result := executeRuntimeTest(app, runtimePreStopConfig(t, 0, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	assert.Empty(t, observation.recordedCalls(), "pre_stop_timeout 0s must not call the hook")
}

// TestPreStopIsSkippedWhenStartupNeverReachedTheRunningState is the other half
// of that guard. An application that failed to start holds nothing -- nothing
// was ever leased or exposed -- so asking it to retract would be asking it to
// undo something that never happened.
func TestPreStopIsSkippedWhenStartupNeverReachedTheRunningState(t *testing.T) {
	observation := &preStopObservation{}
	definition := preStopLiveDefinition("prestop-unstarted", plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error {
			return assert.AnError
		},
		PreStop: func(*runtimeTestValue, context.Context) error {
			observation.record("prestop", context.Background(), context.Background())
			return nil
		},
	})
	app := newRuntimeTestApp(definition)

	result := executeRuntimeTest(app, runtimePreStopConfig(t, time.Second, time.Second)...)
	completed := awaitRuntimeTestResult(t, result)
	require.Error(t, completed.err)
	assert.Empty(t, observation.recordedCalls(),
		"a startup failure before the gate opened never leased anything, so there is nothing to retract")
}

// TestPreStopBudgetExpiresWithoutHoldingUpTheReverseWalk covers the abandon
// path end to end: a hook that ignores its budget must not delay the Stops
// behind it, and the phase must say so rather than exit quietly.
//
// The two phases are timed rather than interleaved, because that is the
// property an operator depends on: the whole stop stays bounded by
// pre_stop_timeout plus shutdown_timeout even when a plugin misbehaves.
func TestPreStopBudgetExpiresWithoutHoldingUpTheReverseWalk(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stopped := make(chan struct{})
	definition := preStopLiveDefinition("prestop-stuck", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error {
			<-release
			return nil
		},
		Stop: func(*runtimeTestValue, context.Context) error {
			close(stopped)
			return nil
		},
	})
	app := newRuntimeTestApp(definition)
	const budget = 100 * time.Millisecond

	result := executeRuntimeTest(app, runtimePreStopConfig(t, budget, 2*time.Second)...)
	awaitRuntimeTestReady(t, app)
	started := time.Now()
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	elapsed := time.Since(started)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)

	select {
	case <-stopped:
	default:
		t.Fatal("the reverse walk must run even though PreStop was abandoned")
	}
	assert.Less(t, elapsed, 2*time.Second,
		"an abandoned PreStop must not consume the shutdown budget, let alone exceed it")
}

// TestPreStopReportReachesTheShutdownReport pins that the phase is not a
// private detail of the shutdown: an operator reading the stop's log has to be
// able to see that the phase ran and what it cost, because the process's total
// stop time is now the sum of two budgets.
//
// It reads the report where an operator does -- the two lines the logger
// carries -- rather than only the settings the two budgets were decoded into. A
// configuration assertion cannot fail when the reporting is removed, and the
// number this test exists for is not either budget: it is their sum, which is
// the ceiling docs/recipes.md tells a supervisor to size TimeoutStopSec and
// stop_grace_period against. A stop that exceeded it would be killed mid-release
// by the very setting the report is supposed to inform, so the sum is asserted
// as it is printed.
func TestPreStopReportReachesTheShutdownReport(t *testing.T) {
	definition := preStopLiveDefinition("prestop-reported", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error { return nil },
		Stop:    func(*runtimeTestValue, context.Context) error { return nil },
	})
	app := newRuntimeTestApp(definition)

	capture, code, err := stopUnderCapture(t, app, 500*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, 0, code)

	assert.Equal(t, 500*time.Millisecond, app.settings.PreStopTimeout)
	assert.Equal(t, time.Second, app.settings.ShutdownTimeout,
		"the two budgets stay independent, so their sum is what a supervisor must allow for")

	phase := preStopEntryFor(t, capture, "xbc: pre-stop phase finished inside its budget")
	assert.Equal(t, "debug", phase.level, "a clean phase is not a warning")
	phaseFields := phase.fields()
	assert.Equal(t, "500ms", phaseFields["budget"], "the line states the budget it ran under")
	waited, ok := phaseFields["waited"].([]string)
	require.True(t, ok, "the phase reports what each hook cost, so a slow retraction is attributable")
	require.Len(t, waited, 1)
	assert.Contains(t, waited[0], "prestop-reported ")

	unwind := preStopEntryFor(t, capture, "xbc: reverse unwind finished inside its budget")
	unwindFields := unwind.fields()
	assert.Equal(t, "1s", unwindFields["budget"], "the walk reports its own budget, not the phase's")
	assert.NotEqual(t, "skipped", unwindFields["pre_stop"],
		"the shutdown line states what the phase actually cost, so the two budgets are not read as one")
	// What that field carries is the phase's cost, not the budget it ran under:
	// a retraction that returned at once has to be distinguishable from a phase
	// that spent its whole allowance, and only the measured cost separates them.
	cost, costErr := time.ParseDuration(unwindFields["pre_stop"].(string))
	require.NoError(t, costErr, "the phase's cost is stated as a duration")
	assert.GreaterOrEqual(t, cost, time.Duration(0))
	assert.Less(t, cost, 500*time.Millisecond,
		"a hook that returned immediately must not be reported as having spent the phase budget")
	assert.Equal(t, (time.Second + 500*time.Millisecond).String(), unwindFields["total_budget"],
		"one stop is pre_stop_timeout plus shutdown_timeout, and the sum is the number a supervisor has to allow for")

	assert.Empty(t, preStopEntriesFor(capture, "xbc: pre-stop hooks did not finish cleanly"),
		"a phase in which every hook returned nil must not warn: a warning that also fires on the healthy case is one operators learn to ignore")
}

// TestPreStopBudgetExpiryIsReportedWithTheAbandonedPlugins pins the one signal
// an operator gets that a hook ignored the phase budget. The hook itself says
// nothing by construction -- it is still running when the process moves on --
// so without this line the only trace of a hung retraction is a stop that took
// exactly as long as the budget.
//
// The assertion discriminates on the abandoned identities and on the emptiness
// of "waited", not on the message alone. A warning that stopped naming the
// plugins, or that reported the budget as a measurement of anything, would still
// log something; and "waited" being empty is the load-bearing half, because an
// abandoned hook's recorded duration is the budget rather than a measurement, so
// listing it would read as a hook that was waited for.
func TestPreStopBudgetExpiryIsReportedWithTheAbandonedPlugins(t *testing.T) {
	// Released only when the test ends: the abandoned hooks are deliberately
	// left running past the phase, which is what "abandoned" means, and closing
	// this is what keeps them from outliving the test binary.
	release := make(chan struct{})
	defer close(release)

	stuck := func(key plugin.Key) plugin.Definition {
		return preStopLiveDefinition(key, plugin.Lifecycle[*runtimeTestValue]{
			PreStop: func(*runtimeTestValue, context.Context) error {
				<-release
				return nil
			},
		})
	}

	app := newRuntimeTestApp(stuck("a-stuck"), stuck("b-stuck"))
	capture, code, err := stopUnderCapture(t, app, 50*time.Millisecond)
	require.NoError(t, err, "an abandoned hook is reported, not turned into a failed run")
	require.Equal(t, 0, code)

	entry := preStopEntryFor(t, capture, "xbc: pre-stop budget expired before every hook returned")
	assert.Equal(t, "warn", entry.level)

	fields := entry.fields()
	assert.Equal(t, (50 * time.Millisecond).String(), fields["budget"])
	assert.ElementsMatch(t, []string{"a-stuck", "b-stuck"}, fields["abandoned"],
		"every hook that ignored its budget must be named, however many there were")
	assert.Empty(t, fields["waited"],
		"nothing was waited for: an abandoned hook's duration is the budget, and reporting it as a wait would be a number nobody measured")
}

// TestPreStopPanicDoesNotStopTheShutdown is the boundary at the level an
// operator experiences it: a plugin that panics while retracting must not take
// the stop flow down with it, and every other plugin must still be stopped.
func TestPreStopPanicDoesNotStopTheShutdown(t *testing.T) {
	stopped := make(chan struct{})
	angry := preStopLiveDefinition("a-angry", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error { panic("release blew up") },
	})
	other := preStopLiveDefinition("b-other", plugin.Lifecycle[*runtimeTestValue]{
		Stop: func(*runtimeTestValue, context.Context) error { close(stopped); return nil },
	})
	app := newRuntimeTestApp(angry, other)

	result := executeRuntimeTest(app, runtimePreStopConfig(t, time.Second, time.Second)...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)

	select {
	case <-stopped:
	default:
		t.Fatal("a panicking PreStop must not prevent other plugins from being stopped")
	}
}

// TestPreStopFailureIsLoggedAndDoesNotFailTheRun pins the deliberate asymmetry
// with a startup hook: a retraction that fails during a shutdown that is
// already under way has no better outcome left to offer, so it is reported and
// the process still exits cleanly.
//
// Both halves are asserted, and the second one is why this test has two
// subtests rather than one. "Is logged" is load-bearing here and not merely a
// description: unlike Stop, a failed PreStop does not fail the run, so the log
// is the only place the failure can appear -- and a check that stopped at the
// exit code would pass with the notification deleted, which is exactly how a
// release that silently did not happen looks from the outside. The two halves
// need different seams (see stopUnderCapture), so they are separate subtests
// rather than one assertion sequence.
func TestPreStopFailureIsLoggedAndDoesNotFailTheRun(t *testing.T) {
	failing := func() plugin.Definition {
		return preStopLiveDefinition("prestop-fails", plugin.Lifecycle[*runtimeTestValue]{
			PreStop: func(*runtimeTestValue, context.Context) error { return assert.AnError },
		})
	}

	t.Run("the run still ends cleanly", func(t *testing.T) {
		app := newRuntimeTestApp(failing())

		result := executeRuntimeTest(app, runtimePreStopConfig(t, time.Second, time.Second)...)
		awaitRuntimeTestReady(t, app)
		app.requestStop(stopReasonSignal)
		completed := awaitRuntimeTestResult(t, result)
		assert.NoError(t, completed.err, "a failed retraction is reported, not turned into a failed run")
		assert.Equal(t, 0, completed.code)
	})

	t.Run("the failure is logged", func(t *testing.T) {
		app := newRuntimeTestApp(failing())

		capture, code, err := stopUnderCapture(t, app, time.Second)
		require.NoError(t, err, "the phase's own failure must not become the run's failure")
		require.Equal(t, 0, code)

		entry := preStopEntryFor(t, capture, "xbc: pre-stop hooks did not finish cleanly")
		assert.Equal(t, "warn", entry.level,
			"a retraction that did not happen is an operational problem, not a debug detail")

		fields := entry.fields()
		assert.Equal(t, time.Second.String(), fields["budget"])
		assert.Equal(t, []string{"prestop-fails"}, fields["failed"],
			"the warning must name which plugin failed, not merely that one did")
		assert.Empty(t, fields["panicked"], "a returned error is not a panic, and the two are never conflated")
		require.Len(t, fields["errors"], 1)
		assert.Contains(t, fields["errors"].([]string)[0], "prestop-fails: ",
			"the cause stays attributed to the plugin that produced it")
		assert.Contains(t, fields["errors"].([]string)[0], assert.AnError.Error(),
			"the hook's own error text has to survive into the line: a plugin name alone does not tell an operator what failed to release")
	})
}

// TestPreStopPanicIsLoggedAndDoesNotStopTheShutdown is the log half of the
// panic boundary, whose behavioural half TestPreStopPanicDoesNotStopTheShutdown
// already pins.
//
// It is a separate case rather than a second assertion in that test because the
// classification is the point: a recovered panic is reported under its own field
// and must never be reported as an ordinary error, so a boundary that stopped
// distinguishing the two would still pass every assertion about the shutdown
// surviving.
func TestPreStopPanicIsLoggedAndDoesNotStopTheShutdown(t *testing.T) {
	defined := preStopLiveDefinition("prestop-angry", plugin.Lifecycle[*runtimeTestValue]{
		PreStop: func(*runtimeTestValue, context.Context) error { panic("release blew up") },
	})
	app := newRuntimeTestApp(defined)

	capture, code, err := stopUnderCapture(t, app, time.Second)
	require.NoError(t, err, "a panic inside one hook must not become the run's failure")
	require.Equal(t, 0, code)

	entry := preStopEntryFor(t, capture, "xbc: pre-stop hooks did not finish cleanly")
	fields := entry.fields()
	assert.Empty(t, fields["failed"], "a panic is not a returned error")
	assert.Equal(t, []string{"prestop-angry"}, fields["panicked"])
	require.Len(t, fields["errors"], 1)
	assert.Contains(t, fields["errors"].([]string)[0], "release blew up",
		"the recovered panic's value is the cause an operator has to see; only the stack behind it is machinery")
}
