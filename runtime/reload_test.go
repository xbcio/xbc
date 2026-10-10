package runtime

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// reloadTestDefinition is a minimal servable plugin: it opens traffic, so a
// boot carrying it reaches the running state. Its framework-owned
// "plugins.reload-guard.enabled" toggle is also the graph-shape leaf the
// reload tests flip when they need a change a reload must refuse.
func reloadTestDefinition() plugin.Definition {
	return plugin.Define("reload-guard", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
}

// reloadTestContents is the quiet base configuration with an explicit log
// level; extra is appended verbatim, indented when it continues the xbc
// section and at column zero when it is a new top-level one.
func reloadTestContents(level string, extra string) string {
	return "log:\n  level: " + level + "\n" +
		"  console:\n    enabled: false\n" +
		"  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: 1s\n" + extra
}

// preserveLogLevel restores the process-global backend level a reload test
// moved, so the next test in the package starts from the ordinary default.
func preserveLogLevel(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _, _ = log.SetLevel("info") })
}

// stopRuntimeTestApp requests a stop and joins the run at most once, so a
// test that already stopped the app still gets its recorded result from the
// cleanup path and a failed assertion cannot leave a watcher running.
func stopRuntimeTestApp(t *testing.T, app *App, result <-chan runtimeTestResult) func() runtimeTestResult {
	t.Helper()
	var final runtimeTestResult
	stopped := false
	stop := func() runtimeTestResult {
		if !stopped {
			stopped = true
			app.requestStop(stopReasonSignal)
			final = awaitRuntimeTestResult(t, result)
		}
		return final
	}
	t.Cleanup(func() { stop() })
	return stop
}

// TestARunningAppReloadsItsLogLevelThroughTheWatch is the end-to-end path: a
// servable process, a file changed on disk, and the running backend observing
// the new level.
func TestARunningAppReloadsItsLogLevelThroughTheWatch(t *testing.T) {
	preserveLogLevel(t)
	path := writeRuntimeTestConfig(t, reloadTestContents("error", ""))
	app := newRuntimeTestApp(reloadTestDefinition())

	result := executeRuntimeTest(app, "--config", path)
	awaitRuntimeTestReady(t, app)
	require.NotNil(t, app.reload.stop, "a servable run over a file must already be watching it")
	stop := stopRuntimeTestApp(t, app, result)

	require.Equal(t, log.ErrorLevel, log.CurrentLevel(), "the boot applied the file's level")

	require.NoError(t, os.WriteFile(path, []byte(reloadTestContents("debug", "")), 0o600))
	require.Eventually(t, func() bool { return log.CurrentLevel() == log.DebugLevel },
		runtimeTestTimeout, 20*time.Millisecond,
		"a level change on disk must reach the running backend")

	completed := stop()
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	require.Nil(t, app.reload.stop, "unwind must stop and clear the watch")
}

// TestReloadOnceAppliesOnlyWhatARunningProcessCanAccept drives reloadOnce
// directly, one rewritten file per case, so the classification is pinned
// without depending on the watch's timing. The level is the observable: a
// case that must be rejected leaves it exactly where the last accepted
// configuration put it.
func TestReloadOnceAppliesOnlyWhatARunningProcessCanAccept(t *testing.T) {
	preserveLogLevel(t)
	path := writeRuntimeTestConfig(t, reloadTestContents("error", ""))
	app := newRuntimeTestApp(reloadTestDefinition())

	result := executeRuntimeTest(app, "--config", path)
	awaitRuntimeTestReady(t, app)
	stop := stopRuntimeTestApp(t, app, result)

	require.Equal(t, log.ErrorLevel, log.CurrentLevel(), "the boot applied the file's level")

	reload := func(contents string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		app.reloadOnce()
	}

	// A graph-shape change rides along with a level change: the whole reload
	// is refused, the level included.
	reload(reloadTestContents("debug", "plugins:\n  reload-guard:\n    enabled: false\n"))
	require.Equal(t, log.ErrorLevel, log.CurrentLevel(),
		"an enabled flip is a graph change: the reload must reject every part of it")

	// The same for the process-level xbc section: none of it can move under a
	// running process.
	reload(reloadTestContents("debug", "  slow_startup_after: 30s\n"))
	require.Equal(t, log.ErrorLevel, log.CurrentLevel(),
		"an xbc change must reject the whole reload")

	// An application-owned change is accepted, and a level change beside it
	// still applies.
	reload(reloadTestContents("warn", "app:\n  greeting: hi\n"))
	require.Equal(t, log.WarnLevel, log.CurrentLevel(),
		"an app.* change must not keep the reloadable part from applying")

	// A rejected reload must not advance the comparison base: the file below
	// differs from the last accepted configuration by log.level alone (the
	// accepted app leaf is gone, which app.* accepts), so it applies.
	reload(reloadTestContents("fatal", "  slow_startup_after: 30s\n"))
	require.Equal(t, log.WarnLevel, log.CurrentLevel(),
		"a file mixing a restart-required path with a level change must be rejected")
	reload(reloadTestContents("fatal", ""))
	require.Equal(t, log.FatalLevel, log.CurrentLevel(),
		"the base must still be the last accepted configuration, not the rejected one")

	// A file that cannot be parsed is refused without touching anything.
	reload("log:\n  level: [unclosed\n")
	require.Equal(t, log.FatalLevel, log.CurrentLevel(),
		"an unloadable file must leave the running configuration alone")

	// Removing the level returns the backend to what the schema defaults to;
	// the removal is itself a change a reload must see.
	reload(runtimeTestConfigContents(time.Second))
	require.Equal(t, log.InfoLevel, log.CurrentLevel(),
		"a removed log.level must reload to the schema's default")

	completed := stop()
	require.NoError(t, completed.err)
}

// TestUnwindStopsTheConfigurationWatchBeforePluginStop pins the shutdown
// order: the watch is stopped and joined before any plugin hook of the
// reverse walk, so a reload cannot apply a change to a graph that is coming
// apart.
func TestUnwindStopsTheConfigurationWatchBeforePluginStop(t *testing.T) {
	preserveLogLevel(t)
	watchedAtStop := make(chan func() error, 1)
	var app *App
	definition := plugin.Define("reload-guard", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		Stop: func(*runtimeTestValue, context.Context) error {
			watchedAtStop <- app.reload.stop
			return nil
		},
	}})
	app = newRuntimeTestApp(definition)

	result := executeRuntimeTest(app, "--config", writeRuntimeTestConfig(t, reloadTestContents("error", "")))
	awaitRuntimeTestReady(t, app)
	require.NotNil(t, app.reload.stop)

	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)

	select {
	case watched := <-watchedAtStop:
		require.Nil(t, watched, "a plugin's Stop ran before the configuration watch was stopped")
	default:
		t.Fatal("the plugin's Stop never ran")
	}
}

// TestReadOnlyCommandsStartNoConfigurationWatch pins the zero-side-effect
// half of the seam: doctor and validate read the configuration but must not
// arm a watch or leave anything running behind them.
func TestReadOnlyCommandsStartNoConfigurationWatch(t *testing.T) {
	defer goleak.VerifyNone(t)
	preserveLogLevel(t)
	path := writeRuntimeTestConfig(t, reloadTestContents("error", ""))

	doctorApp := newRuntimeTestApp(reloadTestDefinition())
	doctorApp.out = io.Discard
	code, err := doctorApp.Execute(context.Background(), []string{"doctor", "--config", path})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Nil(t, doctorApp.reload.stop, "doctor is read-only: it must not start a configuration watch")

	validateApp := newRuntimeTestApp(reloadTestDefinition())
	validateApp.out = io.Discard
	code, err = validateApp.Execute(context.Background(), []string{"validate", "--config", path})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Nil(t, validateApp.reload.stop, "validate serves nothing: it must not start a configuration watch")
}

// TestARunThatNeverReachedServableStartsNoWatch pins the other side of the
// timing: a run whose startup fails after the gate but before it can be left
// running never arms the watch, so unwind has nothing to stop.
func TestARunThatNeverReachedServableStartsNoWatch(t *testing.T) {
	preserveLogLevel(t)
	value := func(plugin.BuildContext) (*runtimeTestValue, error) { return &runtimeTestValue{}, nil }
	app := newRuntimeTestApp(plugin.Define("inert", value, plugin.Options[*runtimeTestValue]{
		Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
			Init: func(*runtimeTestValue, *plugin.Context) error { return nil },
		},
	}))

	result := executeRuntimeTest(app, "--config", writeRuntimeTestConfig(t, reloadTestContents("error", "")))
	completed := awaitRuntimeTestResult(t, result)
	require.Error(t, completed.err)
	assert.Contains(t, completed.err.Error(), "no plugin provides a long-lived capability")
	assert.Nil(t, app.reload.stop, "a run that failed liveness must never have armed the watch")
}
