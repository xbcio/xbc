package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/cli"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
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
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: 1s\n" + extra
	cmd, err := cli.ParseArgs(
		[]string{"doctor", "--config", writeRuntimeTestConfig(t, contents)},
		config.DefaultEnvPrefix)
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
