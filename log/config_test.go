package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigNormalizes(t *testing.T) {
	c := DefaultConfig()
	require.NoError(t, c.Normalize())

	assert.Equal(t, "info", c.Level)
	assert.True(t, c.Caller)
	assert.Equal(t, "error", c.Stacktrace)
	assert.True(t, c.Console.Enabled)
	assert.Equal(t, FormatConsole, c.Console.Format)
	assert.Equal(t, ColorAuto, c.Console.Color)
	assert.False(t, c.File.Enabled, "文件输出默认关闭")
	assert.Equal(t, 100, c.Sampling.Initial)
	assert.Equal(t, 100, c.Sampling.Thereafter)
}

func TestNormalizeIsIdempotent(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.jsonl"

	require.NoError(t, c.Normalize())
	first := c
	require.NoError(t, c.Normalize())
	assert.Equal(t, first, c, "Normalize 必须幂等")
}

// The file suffix itself is the format declaration. Use .jsonl instead of .json --
// the latter makes `jq .` error out on a multi-line file.
func TestFileFormatInferredFromExtension(t *testing.T) {
	cases := map[string]string{
		"logs/app.log":       FormatConsole,
		"logs/app.txt":       FormatConsole,
		"logs/app":           FormatConsole,
		"logs/app.jsonl":     FormatJSON,
		"logs/app.log.jsonl": FormatJSON,
		"logs/app.ndjson":    FormatJSON,
		"logs/app.json":      FormatJSON,
		"LOGS/APP.JSONL":     FormatJSON,
	}
	for path, want := range cases {
		c := DefaultConfig()
		c.File.Enabled = true
		c.File.Path = path
		c.File.Format = ""
		require.NoError(t, c.Normalize(), path)
		assert.Equal(t, want, c.File.Format, "路径 %q", path)
	}
}

func TestExplicitFormatOverridesInference(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.jsonl"
	c.File.Format = FormatConsole
	require.NoError(t, c.Normalize())
	assert.Equal(t, FormatConsole, c.File.Format, "显式配置压过后缀推导")
}

// error_path has its own suffix and its format is inferred independently.
func TestErrorPathFormatInferredIndependently(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.log"
	c.File.ErrorPath = "logs/error.jsonl"
	require.NoError(t, c.Normalize())
	assert.Equal(t, FormatConsole, c.File.Format)
	assert.Equal(t, FormatJSON, c.File.errorFormat)
}

func TestNormalizeRejectsBadEnums(t *testing.T) {
	bad := []struct {
		name   string
		mutate func(*Config)
	}{
		{"level", func(c *Config) { c.Level = "verbose" }},
		{"stacktrace", func(c *Config) { c.Stacktrace = "always" }},
		{"console.format", func(c *Config) { c.Console.Format = "logfmt" }},
		{"console.color", func(c *Config) { c.Console.Color = "maybe" }},
		{"file.format", func(c *Config) { c.File.Enabled = true; c.File.Format = "xml" }},
		{"file.rotate", func(c *Config) { c.File.Enabled = true; c.File.Rotate = "hourly" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mutate(&c)
			assert.Error(t, c.Normalize())
		})
	}
}

func TestFileEnabledWithoutPathGetsDefault(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = ""
	require.NoError(t, c.Normalize())
	assert.Equal(t, "logs/app.log", c.File.Path)
}

func TestNoSinkEnabledIsAllowed(t *testing.T) {
	c := DefaultConfig()
	c.Console.Enabled = false
	c.File.Enabled = false
	assert.NoError(t, c.Normalize(), "全关等价于 Nop，是合法配置（测试环境常用）")
}
