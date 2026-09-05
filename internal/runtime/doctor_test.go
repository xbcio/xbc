package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// runDoctor executes the doctor subcommand against a captured writer and
// returns everything it printed.
func runDoctor(t *testing.T, app *App, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	app.out = &out
	code, err := app.Execute(context.Background(), append([]string{"doctor"}, args...))
	require.NoError(t, err)
	require.Equal(t, 0, code)
	return out.String()
}

// TestDoctorInitializesNoLoggerAndEmitsNoLogLine pins doctor's read-only
// contract at the level that matters operationally: running it against a
// production configuration must not open the log file, must not start the
// flusher goroutine, and must not replace the process-global logger, because
// an operator runs doctor precisely when they are not sure the configuration
// is safe to act on.
func TestDoctorInitializesNoLoggerAndEmitsNoLogLine(t *testing.T) {
	before := log.L()

	definition := plugin.Define("diagnosed", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)
	// A configuration that would have produced console output had the logger
	// actually been installed, so that "no log line" is a real observation.
	args := []string{"--config", writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: true\nxbc:\n  shutdown_timeout: 1s\n")}

	out := runDoctor(t, app, args...)

	assert.Equal(t, before, log.L(), "doctor must not replace the process-global logger")
	assert.Equal(t, log.Nop(), app.logger, "doctor runs against a no-op logger")
	assert.NotContains(t, out, "assembly plan complete",
		"the plan is reported by doctor's own output, never through the logger")
	assert.Contains(t, out, "xbc doctor")
	assert.Contains(t, out, "diagnosed")
}

// TestDoctorReportsGraphInstancesSourcesAndDisableReasons covers the four
// things doctor exists to answer, in one run, because a reader diagnosing "why
// is my plugin off" needs them together.
func TestDoctorReportsGraphInstancesSourcesAndDisableReasons(t *testing.T) {
	on := plugin.Define("switched-on", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-on")})
	off := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-off")})

	app := newRuntimeTestApp(on, off)
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"plugins:\n  switched-on:\n    enabled: true\n")...)

	assert.Contains(t, out, "declared 2, enabled instances 1, disabled 1")
	assert.Contains(t, out, "plugins.switched-on", "an enabled instance names the section it binds")
	assert.Contains(t, out, "switched-off")
	assert.Contains(t, out, "activation path plugins.switched-off is not configured",
		"a disabled plugin must say why, not merely that it is off")
	assert.Contains(t, out, "sources  file ", "doctor names where configuration came from")
}

// TestDoctorPrintsNoConfiguredValue is the secret-safety guard: doctor may
// name paths, identities and source labels, but a configured value -- which in
// a real deployment is a DSN or a token -- must never reach its output.
func TestDoctorPrintsNoConfiguredValue(t *testing.T) {
	const secret = "postgres://user:hunter2@db/app"

	definition := plugin.DefineConfigured("secretive",
		plugin.ConfigSpec[secretiveConfig]{Defaults: func() secretiveConfig { return secretiveConfig{} }},
		func(plugin.BuildContext, secretiveConfig) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		})

	app := newRuntimeTestApp(definition)
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"plugins:\n  secretive:\n    dsn: \""+secret+"\"\n")...)

	assert.NotContains(t, out, secret, "doctor reports where a value came from, never the value")
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, "plugins.secretive", "the path itself is safe and is what the reader needs")
}

type secretiveConfig struct {
	DSN string `yaml:"dsn"`
}

// TestEnvironmentAloneActivatesAWhenConfiguredPlugin is the end-to-end shape of
// the whole task: a container that sets one variable and mounts no
// configuration file must be able to turn a plugin on.
func TestEnvironmentAloneActivatesAWhenConfiguredPlugin(t *testing.T) {
	definition := plugin.Define("env-activated", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.env-activated")})

	off := newRuntimeTestApp(definition)
	require.Contains(t, runDoctor(t, off, runtimeTestConfig(t, time.Second)...),
		"declared 1, enabled instances 0, disabled 1", "without the variable the plugin stays off")

	t.Setenv("XBC_PLUGINS_ENV_ACTIVATED_ENABLED", "true")
	on := newRuntimeTestApp(definition)
	out := runDoctor(t, on, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "declared 1, enabled instances 1, disabled 0",
		"an environment variable alone must be able to activate a WhenConfigured plugin")
	assert.Contains(t, out, "from env", "doctor attributes the activation to the environment layer")
}

// TestEnvironmentAloneDeclaresMultipleInstances pins the multi-instance half of
// the same promise, including that the instance names came from nowhere but the
// environment.
func TestEnvironmentAloneDeclaresMultipleInstances(t *testing.T) {
	definition := plugin.DefineConfigured("env-multi",
		plugin.ConfigSpec[secretiveConfig]{Defaults: func() secretiveConfig { return secretiveConfig{} }},
		func(plugin.BuildContext, secretiveConfig) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		},
		plugin.Options[*runtimeTestValue]{Instances: plugin.MultipleInstances})

	t.Setenv("XBC_PLUGINS_ENV_MULTI_PRIMARY_DSN", "primary")
	t.Setenv("XBC_PLUGINS_ENV_MULTI_REPLICA_DSN", "replica")

	app := newRuntimeTestApp(definition)
	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "plugins.env-multi.primary")
	assert.Contains(t, out, "plugins.env-multi.replica")
	assert.Contains(t, out, "enabled instances 2",
		"instance names are discovered by environment enumeration, with no file involved")
}

// TestUnownedTopLevelKeyFailsBeforeAnythingIsConstructed pins the ownership
// rule at the level an operator meets it: a typo in a top-level section name is
// a startup failure that names the key, not a silently ignored block.
func TestUnownedTopLevelKeyFailsBeforeAnythingIsConstructed(t *testing.T) {
	definition := plugin.Define("owns-nothing-toplevel", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)

	code, err := app.Execute(context.Background(),
		runtimeTestConfigWith(t, time.Second, "wbe:\n  addr: \":8080\"\n"))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wbe", "the error names the offending path")
	assert.Contains(t, err.Error(), "declared top-level sections",
		"the error shows what would have been accepted")
	assert.Nil(t, app.owned, "the failure happens before construction")
}

// TestApplicationFreeformRootIsAcceptedWithoutASchema pins the other side of
// that rule: app.* is declared freeform on purpose, so an application may put
// anything under it without the framework claiming to understand it.
func TestApplicationFreeformRootIsAcceptedWithoutASchema(t *testing.T) {
	definition := plugin.Define("freeform-neighbour", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)

	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"app:\n  name: demo\n  anything:\n    nested: true\n")...)
	assert.Contains(t, out, "app", "the application root is a declared section")
	assert.True(t, strings.Contains(out, "roots"), "doctor lists the declared roots")
}
