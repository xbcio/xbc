package pprof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPluginUsesDisabledDefaults(t *testing.T) {
	p, err := newPlugin(defaultConfig())
	require.NoError(t, err)
	cfg := p.currentSettings()
	assert.False(t, cfg.enabled)
	assert.Equal(t, defaultPath, cfg.path)
}

func TestNewPluginUsesExplicitConfiguration(t *testing.T) {
	cfg := defaultConfig()
	cfg.Enabled = true
	first, err := newPlugin(cfg)
	require.NoError(t, err)
	second, err := newPlugin(cfg)
	require.NoError(t, err)

	require.NotSame(t, first, second)
	assert.True(t, first.currentSettings().enabled)
	assert.True(t, second.currentSettings().enabled)
}
