package runtime

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
)

// writeRuntimeTestConfig writes exactly the given YAML, for the cases whose
// subject is a malformed section that runtimeTestConfigWith's quiet preamble
// would otherwise supply correctly.
func writeRuntimeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func settingsEnvironment(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	env, err := config.NewEnvironment(values, "XBC_SETTINGS_TEST_")
	require.NoError(t, err)
	return env
}

func TestSettingsFallBackToDeclaredDefaultsWithoutAnXbcSection(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, loaded.ShutdownTimeout)
	assert.False(t, loaded.AutoMigrate, "migration is a side-effecting write and is never on by default")
	assert.Equal(t, 30*time.Second, loaded.SlowStartupAfter,
		"the default is positive because a startup that hangs is otherwise silent under the default configuration")
}

func TestSettingsBindEveryDeclaredKeyNotJustTheFirst(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
		"shutdown_timeout":   "5s",
		"auto_migrate":       true,
		"slow_startup_after": "2m",
	}}))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, loaded.ShutdownTimeout)
	assert.True(t, loaded.AutoMigrate)
	assert.Equal(t, 2*time.Minute, loaded.SlowStartupAfter)
}

// TestSettingsAcceptZeroButNotANegativeSlowStartupThreshold pins the asymmetry
// with shutdown_timeout above. Zero is the documented off switch, matching
// web.shutdown.pre_drain_delay and the access log's slow_request, because the
// threshold gates a report and not a deadline: an application with legitimately
// long migrations turns it off rather than being warned on every boot. Negative
// is not an off switch, it is a mistake, and it must not be silently read as
// one.
func TestSettingsAcceptZeroButNotANegativeSlowStartupThreshold(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, map[string]any{
		"xbc": map[string]any{"slow_startup_after": "0s"},
	}))
	require.NoError(t, err)
	assert.Zero(t, loaded.SlowStartupAfter, "zero is how the report is turned off")

	_, err = loadSettings(settingsEnvironment(t, map[string]any{
		"xbc": map[string]any{"slow_startup_after": "-1s"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slow_startup_after")
}

func TestSettingsRejectANonPositiveOrUnparsableShutdownBudget(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0s", "-1s"} {
		_, err := loadSettings(settingsEnvironment(t, map[string]any{
			"xbc": map[string]any{"shutdown_timeout": value},
		}))
		require.Error(t, err, "shutdown_timeout=%s would expire the whole budget immediately", value)
		assert.Contains(t, err.Error(), "must be positive")
	}

	_, err := loadSettings(settingsEnvironment(t, map[string]any{
		"xbc": map[string]any{"shutdown_timeout": "abc"},
	}))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "must be positive",
		"an unparsable duration fails while decoding, not as a silently zeroed value")
}

func TestSettingsRejectUnknownKeysInTheFrameworkSection(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
		"shutdown_timeout":      "5s",
		"totally_unknown_field": "surprise",
	}}))
	require.Error(t, err, "a misspelled framework key must fail loudly instead of defaulting")
	assert.Contains(t, err.Error(), "xbc.totally_unknown_field")
}

func TestSettingsRuntimeKnobsDeclareTheDocumentedDefaults(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)
	assert.Equal(t, maxProcsSetting("auto"), loaded.Runtime.MaxProcs,
		"auto is the default because a container is sized by its quota, not by the host's processors")
	assert.Equal(t, memoryLimitSetting("0"), loaded.Runtime.MemoryLimit)
	assert.Zero(t, loaded.Runtime.GCPercent, "the Go default of 100 is left in place")
}

// TestSettingsBindTheNestedRuntimeSection covers both spellings a deployment
// file uses: the words "auto" and "75%", and the plain integers a YAML author
// writes without quoting. The two travel different paths through the binder --
// the text ones reach UnmarshalText, the integers are converted to text before
// this package ever sees them -- so both have to land on the same parse.
func TestSettingsBindTheNestedRuntimeSection(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
		"runtime": map[string]any{
			"max_procs":    4,
			"memory_limit": "75%",
			"gc_percent":   200,
		},
	}}))
	require.NoError(t, err)
	assert.Equal(t, maxProcsSetting("4"), loaded.Runtime.MaxProcs)
	assert.Equal(t, memoryLimitSetting("75%"), loaded.Runtime.MemoryLimit)
	assert.Equal(t, 200, loaded.Runtime.GCPercent)

	loaded, err = loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
		"runtime": map[string]any{"max_procs": "auto", "memory_limit": 1073741824},
	}}))
	require.NoError(t, err)
	assert.Equal(t, maxProcsSetting("auto"), loaded.Runtime.MaxProcs)
	assert.Equal(t, memoryLimitSetting("1073741824"), loaded.Runtime.MemoryLimit)
}

// TestSettingsPreStopTimeoutDefaultsToAPositiveBudget pins the default that
// makes the stage usable without configuration. It is deliberately positive:
// an application that declares a PreStop hook and forgets the knob would
// otherwise get a phase that never runs, which is exactly the silent failure
// the stage exists to remove.
func TestSettingsPreStopTimeoutDefaultsToAPositiveBudget(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, loaded.PreStopTimeout)
	assert.Equal(t, 30*time.Second, loaded.ShutdownTimeout,
		"the two budgets stay separate, so the worst case for one stop is their sum")
}

// TestSettingsPreStopTimeoutAcceptsZeroButNotNegative pins the asymmetry with
// shutdown_timeout. 0 is the documented off switch for a phase the application
// may simply not need, while negative is not an off switch at all: reading it
// as one would give a deployment that meant to disable the phase the opposite
// of what it asked for.
func TestSettingsPreStopTimeoutAcceptsZeroButNotNegative(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, map[string]any{
		"xbc": map[string]any{"pre_stop_timeout": "0s"},
	}))
	require.NoError(t, err)
	assert.Zero(t, loaded.PreStopTimeout, "0s is how the phase is turned off")

	for _, value := range []string{"-1s", "-1ns"} {
		_, err := loadSettings(settingsEnvironment(t, map[string]any{
			"xbc": map[string]any{"pre_stop_timeout": value},
		}))
		require.Error(t, err, "pre_stop_timeout=%s must be rejected", value)
		assert.Contains(t, err.Error(), "pre_stop_timeout")
		assert.Contains(t, err.Error(), "0s skips the pre-stop phase",
			"the diagnostic has to name the off switch, or the author has no way to say what they meant")
	}
}

// TestSettingsRuntimeKnobsComeFromTheEnvironment pins the environment path for
// this section. The framework's own root already spells the process prefix, so
// it does not repeat it: xbc.runtime.max_procs is XBC_RUNTIME_MAX_PROCS, not
// XBC_XBC_RUNTIME_MAX_PROCS. The bootstrap-level test below shows what the
// doubled spelling does instead of being ignored.
//
// It goes through bootstrap rather than constructing an Environment by hand,
// because resolving a variable to a configuration *path* needs the section
// universe: an Environment built from values alone has no leaves to match a
// name against, so every variable would silently resolve to nothing and the
// assertions below would be reading the declared defaults.
func TestSettingsRuntimeKnobsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("XBC_RUNTIME_MAX_PROCS", "auto")
	t.Setenv("XBC_RUNTIME_MEMORY_LIMIT", "1073741824")
	t.Setenv("XBC_RUNTIME_GC_PERCENT", "50")

	app := newRuntimeTestApp()
	cmd, err := parseArgs(runtimeTestConfig(t, time.Second), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	assert.Equal(t, maxProcsSetting("auto"), app.settings.Runtime.MaxProcs)
	assert.Equal(t, memoryLimitSetting("1073741824"), app.settings.Runtime.MemoryLimit)
	assert.Equal(t, 50, app.settings.Runtime.GCPercent)
}

// TestBootstrapInstallsTheDerivedGOMAXPROCS is this task's delivery boundary: a
// process whose container has a CPU quota ends up sized by that quota rather
// than by the host's processor count. It is deliberately not parallel, because
// it changes a process-global knob and the cgroup root the derivation reads.
func TestBootstrapInstallsTheDerivedGOMAXPROCS(t *testing.T) {
	previousProcs := goruntime.GOMAXPROCS(0)
	t.Cleanup(func() { goruntime.GOMAXPROCS(previousProcs) })
	previousRoot := cgroupRoot
	cgroupRoot = t.TempDir()
	t.Cleanup(func() { cgroupRoot = previousRoot })
	writeCgroupFile(t, cgroupRoot, "cpu.max", "200000 100000")

	app := newRuntimeTestApp()
	cmd, err := parseArgs(runtimeTestConfig(t, time.Second), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	assert.Equal(t, 2, goruntime.GOMAXPROCS(0),
		"the boot must install the quota the container is actually held to")
}

// TestBootstrapInstallsTheConfiguredRuntimeKnobs covers the two remaining
// knobs end to end: the memory limit and the GC percentage are read from the
// environment and installed before any plugin exists.
func TestBootstrapInstallsTheConfiguredRuntimeKnobs(t *testing.T) {
	previousLimit := debug.SetMemoryLimit(-1)
	previousPercent := currentGCPercent()
	t.Cleanup(func() {
		debug.SetMemoryLimit(previousLimit)
		debug.SetGCPercent(previousPercent)
	})
	t.Setenv("XBC_RUNTIME_MEMORY_LIMIT", "1073741824")
	t.Setenv("XBC_RUNTIME_GC_PERCENT", "200")

	app := newRuntimeTestApp()
	cmd, err := parseArgs(runtimeTestConfig(t, time.Second), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	assert.Equal(t, int64(1<<30), debug.SetMemoryLimit(-1))
	assert.Equal(t, 200, currentGCPercent())
}

// TestBootstrapRejectsTheDoubledEnvironmentSpellingForItsOwnSection records
// what the doubled spelling does. The framework's own root already spells the
// process prefix, so XBC_RUNTIME_MAX_PROCS is the name of the field and
// XBC_XBC_RUNTIME_MAX_PROCS names a section that does not exist -- the
// environment layer resolves a reserved variable against the declared sections
// and fails when it names none, so a deployment that doubles the prefix learns
// about the mistake at startup instead of running with a knob nobody set.
func TestBootstrapRejectsTheDoubledEnvironmentSpellingForItsOwnSection(t *testing.T) {
	t.Setenv("XBC_XBC_RUNTIME_MAX_PROCS", "4")

	app := newRuntimeTestApp()
	cmd, err := parseArgs(runtimeTestConfig(t, time.Second), config.DefaultEnvPrefix)
	require.NoError(t, err)
	err = app.bootstrap(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "XBC_XBC_RUNTIME_MAX_PROCS")
}

// TestSettingsRejectAnUnusableRuntimeKnob covers every boundary of the section:
// a value that is neither of the documented forms, an off switch spelled as a
// negative number, and a percentage with no percentage to take.
func TestSettingsRejectAnUnusableRuntimeKnob(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		runtime  map[string]any
		contains string
	}{
		"max_procs is neither auto nor a number": {
			runtime:  map[string]any{"max_procs": "many"},
			contains: "xbc.runtime.max_procs",
		},
		"max_procs is negative": {
			runtime:  map[string]any{"max_procs": -2},
			contains: "xbc.runtime.max_procs",
		},
		"max_procs is empty": {
			runtime:  map[string]any{"max_procs": ""},
			contains: "xbc.runtime.max_procs",
		},
		"memory_limit is a human readable size": {
			runtime:  map[string]any{"memory_limit": "1GiB"},
			contains: "xbc.runtime.memory_limit",
		},
		"memory_limit is negative": {
			runtime:  map[string]any{"memory_limit": -1},
			contains: "xbc.runtime.memory_limit",
		},
		"memory_limit percentage is zero": {
			runtime:  map[string]any{"memory_limit": "0%"},
			contains: "xbc.runtime.memory_limit",
		},
		"memory_limit percentage exceeds the whole limit": {
			runtime:  map[string]any{"memory_limit": "120%"},
			contains: "xbc.runtime.memory_limit",
		},
		"gc_percent is negative": {
			runtime:  map[string]any{"gc_percent": -1},
			contains: "xbc.runtime.gc_percent",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadSettings(settingsEnvironment(t, map[string]any{
				"xbc": map[string]any{"runtime": testCase.runtime},
			}))
			require.Error(t, err, "an unusable runtime knob must fail the boot rather than be ignored")
			assert.Contains(t, err.Error(), testCase.contains,
				"the diagnostic must name the configuration path")
		})
	}
}

// TestSettingsRuntimeKnobDiagnosticsReadLikeSentences pins the house voice:
// one sentence, the suggestion after a semicolon, and no trailing period.
func TestSettingsRuntimeKnobDiagnosticsReadLikeSentences(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(settingsEnvironment(t, map[string]any{
		"xbc": map[string]any{"runtime": map[string]any{"max_procs": -2}},
	}))
	require.Error(t, err)
	assert.Equal(t,
		"xbc: xbc.runtime.max_procs must be \"auto\", a positive processor count, or 0, got \"-2\"; a negative count is not an off switch, write 0 for that",
		err.Error())
}

func TestBootstrapFailuresAreReportedBeforeAnyPlanning(t *testing.T) {
	for name, testCase := range map[string]struct {
		args    []string
		message string
	}{
		"missing configuration file": {
			args:    []string{"--config", filepath.Join(t.TempDir(), "absent.yml")},
			message: "",
		},
		"invalid log level": {
			args:    []string{"--config", writeRuntimeTestConfig(t, "log:\n  level: verbose\n")},
			message: "log",
		},
		"non-positive shutdown budget": {
			args:    []string{"--config", writeRuntimeTestConfig(t, "xbc:\n  shutdown_timeout: 0s\n")},
			message: "must be positive",
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := newRuntimeTestApp()
			code, err := app.Execute(context.Background(), testCase.args)
			assert.Equal(t, 1, code)
			require.Error(t, err)
			if testCase.message != "" {
				assert.Contains(t, err.Error(), testCase.message)
			}
			assert.Nil(t, app.plan, "a bootstrap failure never reaches planning")
		})
	}
}
