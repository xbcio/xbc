package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
)

func TestConfigDefaultsMatchDirectConstruction(t *testing.T) {
	environment, err := config.NewEnvironment(nil, "XBC_WEB_CONFIG_TEST_")
	require.NoError(t, err)

	var bound Config
	require.NoError(t, environment.Bind(ConfigPath, &bound))
	require.NoError(t, config.Validate(&bound, ConfigPath))
	require.NoError(t, bound.Validate())
	assert.Equal(t, DefaultConfig(), bound)

	server := New(nil)
	assert.Equal(t, DefaultConfig(), server.cfg)
}

func TestConfigUsesProductionHTTPAndPayloadDefaults(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, 10*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 5*time.Second, cfg.ReadHeaderTimeout)
	assert.Equal(t, 30*time.Second, cfg.WriteTimeout)
	assert.Equal(t, 60*time.Second, cfg.IdleTimeout)
	assert.Equal(t, 1<<20, cfg.MaxHeaderBytes)
	assert.Equal(t, int64(10<<20), cfg.MaxRequestBodyBytes)
	assert.Equal(t, int64(8<<20), cfg.MaxMultipartMemory)
	assert.Empty(t, cfg.TrustedProxies)
	assert.Zero(t, cfg.Shutdown.PreDrainDelay, "pre-drain is explicit opt-in")
}

func TestConfigRejectsMalformedSecuritySettings(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"relative base path", func(c *Config) { c.BasePath = "api" }},
		{"base path query", func(c *Config) { c.BasePath = "/api?admin=true" }},
		{"zero timeout", func(c *Config) { c.ReadHeaderTimeout = 0 }},
		{"negative body limit", func(c *Config) { c.MaxRequestBodyBytes = -1 }},
		{"negative pre-drain delay", func(c *Config) { c.Shutdown.PreDrainDelay = -time.Second }},
		{"invalid proxy", func(c *Config) { c.TrustedProxies = []string{"proxy.example.com"} }},
		{"invalid cidr", func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/99"} }},
		{"padded proxy", func(c *Config) { c.TrustedProxies = []string{" 127.0.0.1"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.edit(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}

	cfg := DefaultConfig()
	cfg.TrustedProxies = []string{"127.0.0.1", "10.0.0.0/8", "2001:db8::/32"}
	cfg.Shutdown.PreDrainDelay = 250 * time.Millisecond
	assert.NoError(t, cfg.Validate())
}
