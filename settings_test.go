// settings_test.go
package xbc

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
	require.NoError(t, err, "构造内存配置环境失败")
	return env
}

// TestSettingsDefaultsWhenNoXbcSection pins loadSettings' zero-config
// behaviour: with no "xbc:" section at all, ShutdownTimeout comes entirely
// from its `default:"30s"` tag and AutoMigrate stays at its Go zero value.
func TestSettingsDefaultsWhenNoXbcSection(t *testing.T) {
	env := settingsEnv(t, nil)

	s, err := loadSettings(env)
	require.NoError(t, err, "没有 xbc 配置节时 loadSettings 不应该报错")

	assert.Equal(t, 30*time.Second, s.ShutdownTimeout,
		"未配置时 ShutdownTimeout 应落到 default:\"30s\"")
	assert.False(t, s.AutoMigrate, "未配置时 AutoMigrate 应为 false")
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
	require.NoError(t, err, "合法的 xbc 配置节应该被正常绑定")

	assert.Equal(t, 5*time.Second, s.ShutdownTimeout, "shutdown_timeout 应从配置读取为 5s")
	assert.True(t, s.AutoMigrate, "auto_migrate 应从配置读取为 true")
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
			require.Error(t, err, "shutdown_timeout=%s 必须被拒绝：非正数会让整个关闭流程立刻超时", tc.value)
			assert.Contains(t, err.Error(), "必须为正数",
				"错误信息应说明是正数校验失败，而不是别的原因，方便运维一眼看懂")
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
	require.Error(t, err, "非法的 duration 字符串必须返回错误")
	assert.NotContains(t, err.Error(), "必须为正数",
		"非法字符串应该在解析阶段失败，而不是被当成解析成功后的非正数处理")
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
	require.Error(t, err, "xbc 配置节里的未知字段必须在启动期被拒绝")
	assert.Contains(t, err.Error(), "xbc.totally_unknown_field")
}
