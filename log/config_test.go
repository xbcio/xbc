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
	assert.False(t, c.File.Enabled, "File output is default disabled")
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
	assert.Equal(t, first, c, "Normalize must be idempotent")
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
		assert.Equal(t, want, c.File.Format, "Path %q", path)
	}
}

func TestExplicitFormatOverridesInference(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.jsonl"
	c.File.Format = FormatConsole
	require.NoError(t, c.Normalize())
	assert.Equal(t, FormatConsole, c.File.Format, "Explicit configuration overrides suffix derivation")
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
	assert.NoError(t, c.Normalize(), "All off is equivalent to Nop, is a valid configuration (common in test environments)")
}

// TestNormalizeClampsBoundaryValues pins down the 5 numeric clamp points in
// Normalize: each out-of-range value must be snapped back to a documented
// boundary, not silently ignored or rejected.
func TestNormalizeClampsBoundaryValues(t *testing.T) {
	// ── File clamps (only active when File.Enabled == true) ──

	t.Run("MaxSize<=0 clamped to 100", func(t *testing.T) {
		c := DefaultConfig()
		c.File.Enabled = true
		c.File.MaxSize = 0
		require.NoError(t, c.Normalize())
		assert.Equal(t, 100, c.File.MaxSize, "MaxSize=0 must revert to default value 100")

		c.File.MaxSize = -5
		require.NoError(t, c.Normalize())
		assert.Equal(t, 100, c.File.MaxSize, "MaxSize=-5 must revert to default value 100")
	})

	t.Run("MaxAge<0 clamped to 0", func(t *testing.T) {
		c := DefaultConfig()
		c.File.Enabled = true
		c.File.MaxAge = -1
		require.NoError(t, c.Normalize())
		assert.Equal(t, 0, c.File.MaxAge, "MaxAge=-1 must revert to 0 (unlimited)")
	})

	t.Run("MaxBackups<0 clamped to 0", func(t *testing.T) {
		c := DefaultConfig()
		c.File.Enabled = true
		c.File.MaxBackups = -3
		require.NoError(t, c.Normalize())
		assert.Equal(t, 0, c.File.MaxBackups, "MaxBackups=-3 must revert to 0 (unlimited number)")
	})

	// ── Sampling clamps (always active) ──

	t.Run("Sampling.Initial<0 clamped to 0", func(t *testing.T) {
		c := DefaultConfig()
		c.Sampling.Initial = -10
		require.NoError(t, c.Normalize())
		assert.Equal(t, 0, c.Sampling.Initial, "Negative sampling initial value must be clamped to 0 (disable sampling)")
	})

	t.Run("Sampling.Thereafter<0 clamped to 0", func(t *testing.T) {
		c := DefaultConfig()
		c.Sampling.Thereafter = -1
		require.NoError(t, c.Normalize())
		assert.Equal(t, 0, c.Sampling.Thereafter, "Negative sampling subsequent value must be clamped to 0")
	})
}
