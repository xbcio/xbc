package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// signallingCapture is a captureLogger a test can wait on, so that a report
// produced by the watchdog's own goroutine is awaited through a channel instead
// of a sleep. Reading the recorded entries is still only safe after the
// watchdog has been stopped, because stop joins that goroutine.
type signallingCapture struct {
	*captureLogger
	warned chan struct{}
}

func newSignallingCapture() *signallingCapture {
	return &signallingCapture{captureLogger: &captureLogger{}, warned: make(chan struct{}, 8)}
}

func (l *signallingCapture) Warn(msg string, kv ...any) {
	l.captureLogger.Warn(msg, kv...)
	// A non-blocking send: the signal only has to say a report happened, and a
	// watchdog that blocked here could not be joined by stop.
	select {
	case l.warned <- struct{}{}:
	default:
	}
}

func (l *signallingCapture) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-l.warned:
	case <-time.After(runtimeTestTimeout):
		t.Fatalf("the slow startup watchdog did not %s", what)
	}
}

// TestSlowStartupWarningNamesThePluginAndStageItIsStuckIn is the whole point of
// the record. A plugin hook that never returns leaves the startup silent under
// the existing reports: the released-gate line and the per-plugin timing
// breakdown are both emitted after every phase has returned, and an instance's
// StageTiming for the stage in flight is appended only once that stage comes
// back. So on the one boot an operator has to diagnose, every existing
// diagnostic is missing.
//
// A live run is used because the discriminating failure is a mis-wired
// observation: a phase recorded in the wrong place, or a callback the runtime
// forgot to pass to Construct, would still produce a warning -- naming the
// wrong plugin, or none. The warning is rendered through a recorder here rather
// than through the application's own logger, because the watchdog goroutine
// owns that one; both read the same live progress record under its mutex.
func TestSlowStartupWarningNamesThePluginAndStageItIsStuckIn(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	definition := plugin.Define("stuck-start", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error {
			close(entered)
			<-release
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, runtimeTestConfigWith(t, time.Second,
		"  slow_startup_after: 20ms\n")...)
	select {
	case <-entered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("the stuck Start was never entered")
	}

	capture := &captureLogger{}
	app.reportSlowStartup(capture, 90*time.Second, 20*time.Millisecond)

	require.Len(t, capture.entries, 1, "the whole position is one record, so it cannot interleave")
	entry := capture.entries[0]
	assert.Equal(t, "warn", entry.level,
		"a startup that has not finished is not a routine fact, and this record's size does not follow the plugin count")
	assert.Equal(t, "xbc: startup has not finished within the slow startup threshold", entry.msg)

	fields := entry.fields()
	assert.Equal(t, phaseStart, fields["phase"], "the phase names where to look first")
	assert.Equal(t, "stuck-start", fields["plugin"],
		"the plugin holding the startup up is named, which is the field an operator acts on")
	assert.Equal(t, string(assembly.StageStart), fields["stage"],
		"the stage tells a reader which hook to read")
	assert.Equal(t, "1m30s", fields["elapsed"],
		"a duration is reported as a duration, not as a nanosecond count")
	assert.Equal(t, "20ms", fields["threshold"])
	require.IsType(t, "", fields["waiting"])
	waiting, err := time.ParseDuration(fields["waiting"].(string))
	require.NoError(t, err, "how long this position has been in flight is a duration too")
	assert.GreaterOrEqual(t, waiting, time.Duration(0))

	close(release)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)
}

// TestSlowStartupWarningRepeatsWhileStartupIsUnfinished pins periodicity, which
// is what separates "slow but progressing" from "stuck": a single report cannot
// make that distinction, and consecutive reports naming the same plugin and
// stage do. Stopping is asserted in the same place because the two are one
// contract -- a periodic report that outlived the startup would keep claiming an
// application that is already serving has not finished booting.
func TestSlowStartupWarningRepeatsWhileStartupIsUnfinished(t *testing.T) {
	app := newRuntimeTestApp()
	capture := newSignallingCapture()
	app.progress.enterPhase(phaseConstruct)
	app.progress.enterStage(plugin.Identity{Plugin: "slow-factory"}, assembly.StageFactory)

	stop := app.watchSlowStartup(capture, time.Now(), 20*time.Millisecond)
	capture.await(t, "report an unfinished startup")
	capture.await(t, "repeat its report while the startup is still unfinished")
	stop()
	stop()

	entries := capture.entries
	require.GreaterOrEqual(t, len(entries), 2)
	for _, entry := range entries {
		fields := entry.fields()
		assert.Equal(t, "warn", entry.level)
		assert.Equal(t, phaseConstruct, fields["phase"])
		assert.Equal(t, "slow-factory", fields["plugin"],
			"consecutive reports naming the same plugin are what say stuck rather than slow")
		assert.Equal(t, string(assembly.StageFactory), fields["stage"])
	}
}

// TestSlowStartupIsSilentWhenStartupFinishesInsideTheThreshold is the other half
// of the same contract. A warning that fires on every boot is a warning
// operators learn to skip, so the assertion is on level rather than on the
// recorder being empty: the watchdog may report nothing at all, but it must
// never warn about a startup that beat its threshold.
func TestSlowStartupIsSilentWhenStartupFinishesInsideTheThreshold(t *testing.T) {
	definition := plugin.Define("prompt-start", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	capture := &captureLogger{}
	instances := ownForStartupWatch(t, app)

	stop := app.watchSlowStartup(capture, time.Now(), time.Second)
	app.progress.enterPhase(phaseStart)
	require.NoError(t, app.startAll(instances))
	stop()

	for _, entry := range capture.entries {
		assert.NotEqual(t, "warn", entry.level, "a startup that finished inside its threshold must not warn")
	}
}

// TestSlowStartupReportingIsOffWhenTheThresholdIsZero pins the documented
// escape hatch for an application whose migrations legitimately run for
// minutes. Zero must start no watchdog at all rather than merely suppress its
// output, so the position deliberately looks stuck and long overdue: the
// nonzero half of the table proves that same setup does report.
func TestSlowStartupReportingIsOffWhenTheThresholdIsZero(t *testing.T) {
	for name, threshold := range map[string]time.Duration{"disabled": 0, "negative": -time.Second} {
		t.Run(name, func(t *testing.T) {
			app := newRuntimeTestApp()
			capture := newSignallingCapture()
			app.progress.enterPhase(phaseMigrate)
			app.progress.enterStage(plugin.Identity{Plugin: "long-migration"}, assembly.StageMigrate)

			stop := app.watchSlowStartup(capture, time.Now().Add(-time.Hour), threshold)
			stop()
			assert.Empty(t, capture.entries,
				"a disabled threshold reports nothing, however overdue the startup is")
		})
	}

	app := newRuntimeTestApp()
	capture := newSignallingCapture()
	app.progress.enterPhase(phaseMigrate)
	app.progress.enterStage(plugin.Identity{Plugin: "long-migration"}, assembly.StageMigrate)
	stop := app.watchSlowStartup(capture, time.Now().Add(-time.Hour), 20*time.Millisecond)
	capture.await(t, "report with the same position under a positive threshold")
	stop()
	require.NotEmpty(t, capture.entries,
		"the disabled cases above are only meaningful because this identical setup does report")
}

// TestSlowStartupReportFallsSilentOnceTheUnwindBegins pins the boundary of what
// this record may claim. A startup that fails hands straight to the reverse
// unwind, and a migrate-only run unwinds after migrating; the watchdog is still
// running in both. Reporting there would name the phase and plugin of a
// handover that has already come back -- a plugin doing nothing at that moment
// -- while the unwind, which reports itself under its own budget, is the thing
// actually taking the time.
//
// The stop request is deliberately not what silences it, which is why this
// drives a real unwind rather than asserting on the flag: an operator who
// interrupts a boot stuck in a plugin hook requests a stop that cannot be
// honoured, and that is exactly when naming the plugin still holding startup
// matters most.
func TestSlowStartupReportFallsSilentOnceTheUnwindBegins(t *testing.T) {
	definition := plugin.Define("unwound-start", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error { return nil },
		Stop:  func(*runtimeTestValue, context.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	ownUnderCapture(t, app)
	app.progress.enterPhase(phaseStart)
	app.progress.enterStage(plugin.Identity{Plugin: "unwound-start"}, assembly.StageStart)

	underway := &captureLogger{}
	app.reportSlowStartup(underway, time.Minute, time.Second)
	require.Len(t, underway.entries, 1,
		"the position is reportable while the startup is still the thing being waited on")

	require.NoError(t, app.unwind(stopReasonStartupFailed))

	unwound := &captureLogger{}
	app.reportSlowStartup(unwound, 2*time.Minute, time.Second)
	assert.Empty(t, unwound.entries,
		"once the unwind owns the process, a startup position is a stale reading and reporting it contradicts the shutdown report")
}

// TestStartupPhaseChangeForgetsThePreviousPhasesPlugin pins the one way this
// record can lie. The plugin and stage describe a handover the phase has
// already come back from, so carrying them into the next phase would report
// "phase migrate" beside a plugin that is, at that moment, doing nothing --
// a contradiction the reader has no way to detect.
func TestStartupPhaseChangeForgetsThePreviousPhasesPlugin(t *testing.T) {
	t.Parallel()
	var progress startupProgress
	progress.enterPhase(phaseConstruct)
	progress.enterStage(plugin.Identity{Plugin: "gorm", Instance: "primary"}, assembly.StageInit)

	position := progress.position()
	assert.Equal(t, "gorm[primary]", position.identity.String())
	assert.Equal(t, assembly.StageInit, position.stage)

	progress.enterPhase(phaseMigrate)
	position = progress.position()
	assert.Equal(t, phaseMigrate, position.phase)
	assert.Empty(t, position.identity.String(), "a phase that has reached no plugin names none")
	assert.Empty(t, string(position.stage))
	assert.GreaterOrEqual(t, time.Since(position.since), time.Duration(0),
		"entering a phase restarts how long the current position has been in flight")
}

// ownForStartupWatch bootstraps app under a quiet logger and constructs its
// plan with the same stage observation execute installs, so a test can drive
// one startup phase directly without re-deriving the wiring under test.
func ownForStartupWatch(t *testing.T, app *App) []*assembly.Instance {
	t.Helper()
	plan, _ := planUnderCapture(t, app, "")
	owned, err := assembly.Construct(plan, assembly.ConstructOptions{
		ShutdownTimeout: app.settings.ShutdownTimeout,
		OnStageBegin:    app.progress.enterStage,
	})
	require.NoError(t, err)
	app.owned = owned
	return owned.Instances()
}
