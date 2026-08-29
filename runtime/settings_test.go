package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
)

// settingsEnv builds a *config.Environment directly from an in-memory map,
// bypassing file/profile lookup entirely -- config.NewEnvironment's stated
// purpose. Each test gets its own private ENV prefix namespace so a stray
// XBC_SETTINGS_TEST_* variable in the real process environment (there
// should never be one, but Bind does consult os.LookupEnv) cannot leak
// between this file's tests and any other file's.
func settingsEnv(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	env, err := config.NewEnvironment(values, "XBC_SETTINGS_TEST_")
	require.NoError(t, err, "Failed to construct memory configuration environment")
	return env
}

// TestSettingsDefaultsWhenNoXbcSection pins loadSettings' zero-config
// behaviour: with no "xbc:" section at all, ShutdownTimeout comes entirely
// from its `default:"30s"` tag and AutoMigrate stays at its Go zero value.
func TestSettingsDefaultsWhenNoXbcSection(t *testing.T) {
	env := settingsEnv(t, nil)

	s, err := loadSettings(env)
	require.NoError(t, err, "loadSettings should not error when xbc configuration section is missing")

	assert.Equal(t, 30*time.Second, s.ShutdownTimeout,
		"ShutdownTimeout should fall to default:\"30s\" when not configured")
	assert.False(t, s.AutoMigrate, "AutoMigrate should be false when not configured")
}

// TestSettingsReadsShutdownTimeoutAndAutoMigrateFromConfig pins that an
// explicit "xbc:" section actually overrides both defaults, not just one of
// them (a bug that bound only the first field and left the second at its
// default would still pass a test that checked only one).
func TestSettingsReadsShutdownTimeoutAndAutoMigrateFromConfig(t *testing.T) {
	env := settingsEnv(t, map[string]any{
		"xbc": map[string]any{
			"shutdown_timeout": "5s",
			"auto_migrate":     true,
		},
	})

	s, err := loadSettings(env)
	require.NoError(t, err, "Valid xbc configuration section should be properly bound")

	assert.Equal(t, 5*time.Second, s.ShutdownTimeout, "shutdown_timeout should be read from configuration as 5s")
	assert.True(t, s.AutoMigrate, "auto_migrate should be read from configuration as true")
}

// TestSettingsRejectsNonPositiveShutdownTimeout pins loadSettings' explicit
// positivity check. This matters far beyond "reject a weird value": a
// non-positive ShutdownTimeout becomes the total budget the whole shutdown
// path passes down as a context.WithTimeout, and 0s/negative durations both
// produce an already-expired context -- every plugin's Stop would be called
// with zero grace, i.e. graceful shutdown would exist in name only for the
// entire remaining lifetime of the process.
func TestSettingsRejectsNonPositiveShutdownTimeout(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"zero", "0s"},
		{"negative", "-1s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := settingsEnv(t, map[string]any{
				"xbc": map[string]any{"shutdown_timeout": tc.value},
			})

			_, err := loadSettings(env)
			require.Error(t, err, "shutdown_timeout=%s must be rejected: non-positive value will cause the entire shutdown process to timeout immediately", tc.value)
			assert.Contains(t, err.Error(), "must be positive",
				"Error message should indicate it's a positive number validation failure, not another reason, to help operations understand quickly")
		})
	}
}

// TestSettingsRejectsUnparsableShutdownTimeoutString pins that a value that
// is not a valid Go duration string fails at the parse/decode step, before
// loadSettings' own positivity check ever runs. The two failure modes are
// deliberately told apart: NotContains guards against a bug where the
// decode error is discarded and s.ShutdownTimeout silently becomes 0 (which
// would then, incorrectly, surface as the *positivity* error instead of a
// parse error).
func TestSettingsRejectsUnparsableShutdownTimeoutString(t *testing.T) {
	env := settingsEnv(t, map[string]any{
		"xbc": map[string]any{"shutdown_timeout": "abc"},
	})

	_, err := loadSettings(env)
	require.Error(t, err, "Illegal duration string must return an error")
	assert.NotContains(t, err.Error(), "must be positive",
		"Illegal string should fail during parsing, not be treated as non-positive after parsing succeeds")
}

// TestSettingsRejectsUnknownFieldsInXbcSection keeps framework-owned typed
// configuration strict. A misspelled shutdown timeout must fail at startup
// rather than silently falling back to the default and surprising operators.
func TestSettingsRejectsUnknownFieldsInXbcSection(t *testing.T) {
	env := settingsEnv(t, map[string]any{
		"xbc": map[string]any{
			"shutdown_timeout":      "5s",
			"totally_unknown_field": "surprise",
		},
	})

	_, err := loadSettings(env)
	require.Error(t, err, "Unknown fields in xbc configuration section must be rejected during startup")
	assert.Contains(t, err.Error(), "xbc.totally_unknown_field")
}
