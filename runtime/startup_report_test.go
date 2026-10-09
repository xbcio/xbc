package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// captureLogger records what was logged instead of writing it anywhere, so a
// test can assert on both what an operator is told and what must never reach
// them.
type captureLogger struct {
	entries []captureEntry
}

type captureEntry struct {
	level string
	msg   string
	kv    []any
}

func (l *captureLogger) add(level, msg string, kv ...any) {
	l.entries = append(l.entries, captureEntry{level: level, msg: msg, kv: kv})
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) log.Logger      { return l }
func (*captureLogger) Enabled(log.Level) bool        { return true }

// text renders everything the logger was given, message and structured fields
// alike, so that a no-secrets assertion covers the fields too rather than only
// the formatted message.
func (l *captureLogger) text() string {
	var b strings.Builder
	for _, entry := range l.entries {
		fmt.Fprintf(&b, "%s %s", entry.level, entry.msg)
		for _, value := range entry.kv {
			fmt.Fprintf(&b, " %v", value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// planUnderCapture bootstraps app against contents without installing a real
// logger, replaces its logger with a capture, and returns the plan a run would
// have reported on. It borrows the doctor subcommand purely because that is
// the branch bootstrap uses to skip log.Init; nothing here runs doctor.
func planUnderCapture(t *testing.T, app *App, extra string) (*assembly.Plan, *captureLogger) {
	t.Helper()
	// drain_timeout is left unset: it derives as 60% of the fixed 1s
	// shutdown_timeout, which is always strictly smaller than it; these tests
	// are not about the drain phase.
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: 1s\n" + extra
	cmd, err := parseArgs(
		[]string{"doctor", "--config", writeRuntimeTestConfig(t, contents)},
		config.DefaultEnvPrefix, nil)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	plan, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: app.bundles, Env: app.env, Logger: app.logger})
	require.NoError(t, err)

	capture := &captureLogger{}
	app.logger = capture
	return plan, capture
}

// TestReportDisabledNamesEveryDisabledPluginAndWhy pins the half of the
// startup report that turns "I selected the plugin and nothing happened" into
// a statement an operator can act on. A normal boot prints no doctor output at
// all, so this line is the only place a disabled plugin is accounted for.
func TestReportDisabledNamesEveryDisabledPluginAndWhy(t *testing.T) {
	unconfigured := plugin.Define("unconfigured", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.unconfigured")})
	switchedOff := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	stayingOn := plugin.Define("staying-on", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(unconfigured, switchedOff, stayingOn)
	plan, capture := planUnderCapture(t, app, "plugins:\n  switched-off:\n    enabled: false\n")

	app.reportDisabled(plan)

	require.Len(t, capture.entries, 1, "the whole report is one record, so it cannot be split across interleaved lines")
	entry := capture.entries[0]
	assert.Equal(t, "info", entry.level, "being disabled is a normal outcome, not a warning")
	assert.Contains(t, entry.msg, "disabled plugins (2)")
	assert.Contains(t, entry.msg, "unconfigured — activation path plugins.unconfigured is not configured")
	assert.Contains(t, entry.msg, "switched-off — plugins.switched-off.enabled is false")
	assert.Contains(t, entry.msg, "run the doctor subcommand",
		"the report must point at where the full picture lives")
	assert.NotContains(t, entry.msg, "staying-on", "an enabled plugin has nothing to explain")
}

// TestReportDisabledSaysNothingWhenEverythingIsEnabled pins the quiet case: a
// healthy boot must not emit an empty diagnostic that readers learn to ignore.
func TestReportDisabledSaysNothingWhenEverythingIsEnabled(t *testing.T) {
	definition := plugin.Define("all-on", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(definition)
	plan, capture := planUnderCapture(t, app, "")

	app.reportDisabled(plan)

	assert.Empty(t, capture.entries, "nothing disabled means nothing to report")
}

// TestReportDisabledPrintsNoConfiguredValue is the secret-safety guard for the
// startup path, matching the one doctor already carries. A plugin that was
// turned off still has its configuration block sitting in the merged view --
// DSNs and tokens included -- and naming the plugin must never drag those
// along.
func TestReportDisabledPrintsNoConfiguredValue(t *testing.T) {
	const secret = "postgres://user:hunter2@db/app"

	definition := plugin.DefineConfigured("secretive",
		plugin.ConfigSpec[secretiveConfig]{Defaults: func() secretiveConfig { return secretiveConfig{} }},
		func(plugin.BuildContext, secretiveConfig) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		})

	app := newRuntimeTestApp(definition)
	plan, capture := planUnderCapture(t, app,
		"plugins:\n  secretive:\n    enabled: false\n    dsn: \""+secret+"\"\n")

	app.reportDisabled(plan)

	logged := capture.text()
	require.Contains(t, logged, "secretive", "the plugin is named, which is what makes the omission meaningful")
	assert.NotContains(t, logged, secret, "the report names paths and reasons, never a configured value")
	assert.NotContains(t, logged, "hunter2")
}

// startupProbe is the shortest stage duration that survives a coarse clock.
// Every assertion below compares against it with >=, never with equality.
const startupProbe = 20 * time.Millisecond

// TestStartupTimingsAttributeASlowBootToItsPhaseAndPlugin is the whole point of
// the measurement: an application that takes seconds to become servable can
// today only be diagnosed by guessing, because the released-gate line reports
// what started and in what order but never what any of it cost.
//
// A real run is used rather than a hand-built report because the discriminating
// failure is a mis-anchored measurement -- a total taken after the slow phase,
// or a phase timed around the wrong call -- and only the live wiring can catch
// that. The slow work sits in Start, so the assertion also fails if every
// phase were assigned the same elapsed span.
func TestStartupTimingsAttributeASlowBootToItsPhaseAndPlugin(t *testing.T) {
	definition := plugin.Define("slow-start", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(*runtimeTestValue, *plugin.Context) error {
			time.Sleep(startupProbe)
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)

	assert.GreaterOrEqual(t, app.startup.phases.start, startupProbe,
		"the slow phase is the one that was slow")
	assert.GreaterOrEqual(t, app.startup.total, app.startup.phases.start,
		"the total covers the phases it decomposes into")
	assert.NotZero(t, app.startup.phases.bootstrap,
		"configuration bootstrap is measured too, or a slow config source would look like a slow plugin")
	assert.NotZero(t, app.startup.phases.construct)
	assert.Zero(t, app.startup.phases.migrate, "this run was not asked to migrate")

	instance, ok := app.owned.Instance(plugin.Identity{Plugin: "slow-start", Instance: plugin.DefaultInstance})
	require.True(t, ok)
	stages := make(map[assembly.Stage]time.Duration)
	for _, timing := range instance.Timings() {
		stages[timing.Stage] = timing.Duration
	}
	assert.GreaterOrEqual(t, stages[assembly.StageStart], startupProbe,
		"the plugin and the stage inside the slow phase are both named")
	assert.Contains(t, stages, assembly.StageOpenTraffic)
	assert.NotContains(t, stages, assembly.StageMigrate,
		"a stage the Definition never declared did not run")

	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)
}

// TestValidationTimingsAttributeASlowValidateToItsPluginStage is the same
// measurement for the command that stops at validation: the hook is the only
// work validate does beyond construction, so a slow Preflight must reach both
// the total an operator reads and the per-stage breakdown they read it against.
// A real run is used for the same reason as above -- a mis-anchored total or a
// stage timed around the wrong call only fails against the live wiring.
func TestValidationTimingsAttributeASlowValidateToItsPluginStage(t *testing.T) {
	definition := plugin.Define("slow-preflight", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Preflight: func(*runtimeTestValue, *plugin.Context) error {
			time.Sleep(startupProbe)
			return nil
		},
	}})

	app := newRuntimeTestApp(definition)
	result := executeRuntimeTest(app, append([]string{"validate"}, runtimeTestConfig(t, time.Second)...)...)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	require.Equal(t, 0, completed.code)

	assert.GreaterOrEqual(t, app.validation, startupProbe,
		"the reported total covers the hook that was slow")
	instance, ok := app.owned.Instance(plugin.Identity{Plugin: "slow-preflight", Instance: plugin.DefaultInstance})
	require.True(t, ok)
	stages := make(map[assembly.Stage]time.Duration)
	for _, timing := range instance.Timings() {
		stages[timing.Stage] = timing.Duration
	}
	assert.GreaterOrEqual(t, stages[assembly.StagePreflight], startupProbe,
		"the plugin and the stage inside it are both named")
	assert.Zero(t, app.startup.total,
		"validate never fills the boot measurement: the two numbers describe different commands")
}

// TestReportStartedStatesHowLongTheBootTook pins the always-on half. The
// breakdown is debug-only, so this one field is all an operator has on a
// default-configured production boot, and it must be there even when nothing
// was slow.
func TestReportStartedStatesHowLongTheBootTook(t *testing.T) {
	definition := plugin.Define("timed", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(definition)
	_, capture := planUnderCapture(t, app, "")
	app.startup = startupTiming{total: 812 * time.Millisecond}

	app.reportStarted(nil, false)

	require.Len(t, capture.entries, 1)
	entry := capture.entries[0]
	assert.Equal(t, "info", entry.level)
	assert.Equal(t, "812ms", entry.fields()["startup"],
		"a boot duration is reported as a duration, not as a nanosecond count")
}

// TestReportValidatedStatesHowLongValidationTook pins the always-on half of the
// validate command's report, the counterpart of the released-gate line: a CI job
// or a pre-deployment check gets one line saying how many instances were
// checked and how long it took, whatever the log level, because the breakdown
// below it costs a plugin count and stays at debug.
func TestReportValidatedStatesHowLongValidationTook(t *testing.T) {
	definition := plugin.Define("validated", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(definition)
	capture := ownUnderCapture(t, app)
	app.validation = 640 * time.Millisecond

	app.reportValidated(app.owned.Instances())

	require.Len(t, capture.entries, 1)
	entry := capture.entries[0]
	assert.Equal(t, "info", entry.level, "finishing validation is the command's normal outcome")
	assert.Equal(t, "xbc: application validation finished", entry.msg)
	assert.EqualValues(t, 1, entry.fields()["instances"])
	assert.Equal(t, "640ms", entry.fields()["validation"],
		"a validation duration is reported as a duration, not as a nanosecond count")
}

// TestStartupTimingsBreakdownNamesEveryStageThatRan proves the debug entry
// carries what the aggregate cannot: one line per instance, in start order,
// listing the stages that actually ran. It also pins the phase line, which is
// what tells a reader whether to look at plugins at all.
func TestStartupTimingsBreakdownNamesEveryStageThatRan(t *testing.T) {
	definition := plugin.Define("broken-down", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Init: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	capture := ownUnderCapture(t, app)
	app.startup = startupTiming{
		total:  time.Second,
		phases: startupPhases{bootstrap: 21 * time.Millisecond, construct: 128 * time.Millisecond},
	}

	app.reportStartupTimings(app.owned.Instances())

	require.Len(t, capture.entries, 1, "the whole breakdown is one record, so it cannot interleave")
	entry := capture.entries[0]
	assert.Equal(t, "debug", entry.level, "a normal boot must not spend a warning on its own timings")
	assert.Contains(t, entry.msg, "total 1s")
	assert.Contains(t, entry.msg, "bootstrap 21ms")
	assert.Contains(t, entry.msg, "construct 128ms")
	assert.Contains(t, entry.msg, "migrate 0s", "a phase that did not run is stated, not omitted")
	assert.Contains(t, entry.msg, "broken-down")
	assert.Contains(t, entry.msg, "factory ")
	assert.Contains(t, entry.msg, "Init ")
	assert.NotContains(t, entry.msg, "Start ",
		"an undeclared hook must not appear as a stage that ran instantly")
}

// TestStartupTimingsAreNotBuiltWhenDebugIsOff keeps the breakdown's cost
// proportional to its usefulness: it is the one report whose size follows the
// plugin count, and a production logger set to info must not pay for a string
// it will discard.
func TestStartupTimingsAreNotBuiltWhenDebugIsOff(t *testing.T) {
	definition := plugin.Define("quiet", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(definition)
	capture := ownUnderCapture(t, app)
	app.logger = &levelledCapture{captureLogger: capture, enabled: log.InfoLevel}

	app.reportStartupTimings(app.owned.Instances())

	assert.Empty(t, capture.entries)
}

// TestReportValidationTimingsNameOnlyWhatValidateRan pins the debug entry:
// one line per instance whose Preflight hook ran, and nothing for an instance
// that declared none. Preflight is the only stage validate runs of its own, so
// the boot report's other stages are deliberately absent rather than printed as
// zero -- a line here says "this plugin cost this much to check", and an
// instance with nothing to check has nothing to say.
func TestReportValidationTimingsNameOnlyWhatValidateRan(t *testing.T) {
	preflighting := plugin.Define("preflighting", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Init:      func(*runtimeTestValue, *plugin.Context) error { return nil },
		Preflight: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	plain := plugin.Define("plain", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Init: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(preflighting, plain)
	capture := ownUnderCapture(t, app)
	require.NoError(t, app.preflightAll(app.owned.Instances()))
	app.validation = 3 * time.Second

	app.reportValidationTimings(app.owned.Instances())

	require.Len(t, capture.entries, 1, "the whole breakdown is one record, so it cannot interleave")
	entry := capture.entries[0]
	assert.Equal(t, "debug", entry.level, "a normal validation must not spend a warning on its own timings")
	assert.Contains(t, entry.msg, "total 3s")
	assert.Contains(t, entry.msg, "preflighting")
	assert.Contains(t, entry.msg, "Preflight ")
	assert.NotContains(t, entry.msg, "plain",
		"an instance that declared no Preflight hook contributes no line")
	assert.NotContains(t, entry.msg, "Init ",
		"construction is measured by the boot report; it is not validate's own stage")
}

// TestValidationTimingsAreNotBuiltWhenDebugIsOff is the cost guard for the
// validate breakdown, matching the boot one: its size follows the plugin count,
// so a production logger set to info must not pay for a string it discards.
func TestValidationTimingsAreNotBuiltWhenDebugIsOff(t *testing.T) {
	definition := plugin.Define("quiet-preflight", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Preflight: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	capture := ownUnderCapture(t, app)
	app.logger = &levelledCapture{captureLogger: capture, enabled: log.InfoLevel}
	app.validation = time.Second

	app.reportValidationTimings(app.owned.Instances())

	assert.Empty(t, capture.entries)
}

// levelledCapture is a captureLogger that answers Enabled honestly, which the
// plain recorder deliberately does not.
type levelledCapture struct {
	*captureLogger
	enabled log.Level
}

func (l *levelledCapture) Enabled(level log.Level) bool { return level >= l.enabled }

// startOneResidentTaskUnderBudget composes one workload-scoped plugin whose
// Start submits a single process-lifetime task, spells the workload's budget
// out in the configuration bootstrap reads, and runs the Start phase with a
// recording logger, returning what that phase logged.
//
// The phases are driven by hand the way this package's other report tests drive
// them, because the warning under test is emitted from inside the Start phase
// and a full run installs a logger of its own. Everything else is real: the
// limit comes from the workloads root, and the attribution and the hosted set
// come from the plan this composition built.
func startOneResidentTaskUnderBudget(t *testing.T, budget string) (*captureLogger, *App) {
	t.Helper()
	definition := plugin.Define("dispatcher", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			ctx.GoCritical(func(taskCtx context.Context) { <-taskCtx.Done() })
			return nil
		},
	}})

	app := newApp([]plugin.Bundle{
		plugin.WorkloadOf("sast", plugin.BundleOf(definition), plugin.WithReplicas(1)),
	})
	plan, capture := planUnderCapture(t, app, "workloads:\n  sast:\n    max_goroutines: "+budget+"\n")
	app.plan = plan
	owned, err := assembly.Construct(plan, assembly.ConstructOptions{
		ShutdownTimeout: app.settings.ShutdownTimeout,
		// The submission under test goes through Context, so the Context must be
		// the runtime's own host: the same adapter a run installs, or Start would
		// receive no Context at all and the workload would never be charged.
		ContextFactory: func(identity plugin.Identity, logger log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(hostAdapter{app: app, logger: logger}, identity)
		},
	})
	require.NoError(t, err)
	app.owned = owned

	require.NoError(t, app.startAll(owned.Instances()))
	// The resident task runs until its plugin's tasks are torn down; draining is
	// what the drain phase would do with it, and it is what keeps this test from
	// leaving a goroutine behind.
	t.Cleanup(func() {
		app.tasks.closeAdmission()
		require.NoError(t, app.tasks.drainRemaining(context.Background()))
	})
	return capture, app
}

// TestAWorkloadWhoseBudgetStartSpendsIsReported is the only signal an operator
// gets for the silent half of the budget. An admission waits for a unit instead
// of refusing, so a workload whose own process-lifetime tasks have filled its
// limit never runs the queue delivery or the pooled task that was meant to
// charge it, and nothing anywhere says so.
//
// The check runs at the end of Start, so the test asserts on what that phase
// logged rather than on a separately invoked report: deleting the call would
// leave a budget with no diagnostic at all.
func TestAWorkloadWhoseBudgetStartSpendsIsReported(t *testing.T) {
	capture, _ := startOneResidentTaskUnderBudget(t, "1")

	require.Len(t, capture.entries, 1, "one saturated workload is one record")
	entry := capture.entries[0]
	assert.Equal(t, "warn", entry.level, "a workload with nothing left to admit is not a routine fact")
	assert.Equal(t, "xbc: workload goroutine budget fully held at the end of start; an admission for it waits until a held unit comes free", entry.msg)

	fields := entry.fields()
	assert.Equal(t, "sast", fields["workload"], "the workload is named, which is the field an operator acts on")
	assert.Equal(t, 1, fields["limit"])
	assert.Equal(t, 1, fields["running"],
		"the count says whether the limit is the problem or the workload's own start-up work is")
}

// TestAWorkloadWithBudgetToSpareIsNotReported is the other half of the
// contract: the ordinary configuration -- a workload whose resident work leaves
// room for whatever arrives through admission -- must boot without saying
// anything, or readers learn to ignore the line that matters.
func TestAWorkloadWithBudgetToSpareIsNotReported(t *testing.T) {
	capture, _ := startOneResidentTaskUnderBudget(t, "2")

	assert.Empty(t, capture.entries,
		"a workload with a unit to spare is the ordinary case and is not reported")
}
