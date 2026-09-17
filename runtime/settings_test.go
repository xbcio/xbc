package runtime

import (
	"context"
	"os"
	"path/filepath"
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
