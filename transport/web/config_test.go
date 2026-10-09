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
	assert.Zero(t, cfg.MaxInFlight, "zero means the fixed default ceiling; a deployment sets a positive value to replace it")
}

// TestMaxInFlightBindsFromTheWebSection pins the configuration key itself. The
// field is the only thing the ceiling can be read from, so a rename that
// silently stops binding would leave every deployment that sets it running at
// the default limit instead -- a behaviour change with no error attached.
func TestMaxInFlightBindsFromTheWebSection(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"web": map[string]any{"max_in_flight": 64},
	}, "XBC_WEB_MAX_IN_FLIGHT_TEST_")
	require.NoError(t, err)

	var bound Config
	require.NoError(t, environment.Bind(ConfigPath, &bound))
	require.NoError(t, config.Validate(&bound, ConfigPath))
	assert.Equal(t, 64, bound.MaxInFlight)
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
		{"negative in-flight limit", func(c *Config) { c.MaxInFlight = -1 }},
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
	cfg.MaxInFlight = 64
	assert.NoError(t, cfg.Validate())
}

// TestManagementAndTLSBindFromTheWebSection pins the configuration keys
// themselves. The fields are the only thing either section can be read from,
// so a rename that silently stopped binding would leave a deployment that
// configured a management port or a certificate serving the default shape --
// plain HTTP, no operator listener -- with no error attached.
func TestManagementAndTLSBindFromTheWebSection(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"web": map[string]any{
			"management": map[string]any{
				"addr":         "127.0.0.1:9091",
				"allow_remote": false,
			},
			"tls": map[string]any{
				"cert_file":       "/etc/tls/tls.crt",
				"key_file":        "/etc/tls/tls.key",
				"min_version":     "1.3",
				"client_ca_file":  "/etc/tls/ca.crt",
				"client_auth":     "require_and_verify",
				"cipher_suites":   []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
				"reload_interval": "30s",
			},
		},
	}, "XBC_WEB_MANAGEMENT_TLS_TEST_")
	require.NoError(t, err)

	var bound Config
	require.NoError(t, environment.Bind(ConfigPath, &bound))
	require.NoError(t, config.Validate(&bound, ConfigPath))
	require.NoError(t, bound.Validate())
	assert.Equal(t, "127.0.0.1:9091", bound.Management.Addr)
	assert.False(t, bound.Management.AllowRemote)
	assert.Equal(t, "/etc/tls/tls.crt", bound.TLS.CertFile)
	assert.Equal(t, "/etc/tls/tls.key", bound.TLS.KeyFile)
	assert.Equal(t, "1.3", bound.TLS.MinVersion)
	assert.Equal(t, "/etc/tls/ca.crt", bound.TLS.ClientCAFile)
	assert.Equal(t, "require_and_verify", bound.TLS.ClientAuth)
	assert.Equal(t, []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}, bound.TLS.CipherSuites)
	assert.Equal(t, 30*time.Second, bound.TLS.ReloadInterval)
}

// TestConfigAcceptsAManagementListenerOnlyBehindItsAccessRule is the positive
// half of management validation: a loopback address is always fine, and a
// remote one is fine exactly when it is declared remote and the process
// terminates TLS itself.
func TestConfigAcceptsAManagementListenerOnlyBehindItsAccessRule(t *testing.T) {
	for _, addr := range []string{":9091", "127.0.0.1:9091", "localhost:9091", "[::1]:9091"} {
		cfg := DefaultConfig()
		cfg.Management.Addr = addr
		assert.NoErrorf(t, cfg.Validate(), "%s names this machine only and needs no declaration", addr)
	}

	// allow_remote declares that the listener may be reached from somewhere
	// other than this machine, so it demands the material that makes such a
	// listener safe whatever address is spelled beside it -- including a
	// loopback one, where the declaration is inert rather than harmless.
	cfg := DefaultConfig()
	cfg.Management.Addr = ":9091"
	cfg.Management.AllowRemote = true
	assert.Error(t, cfg.Validate(), "allow_remote requires a certificate even on a loopback address")

	cfg = DefaultConfig()
	cfg.Management.Addr = "0.0.0.0:9091"
	cfg.Management.AllowRemote = true
	cfg.TLS = TLSConfig{CertFile: "/etc/tls/tls.crt", KeyFile: "/etc/tls/tls.key"}
	assert.NoError(t, cfg.Validate(), "a remote management address with TLS is the declared-remote case")
}

// TestManagementAddressBindsThroughItsTag covers the binder path for the
// address rule itself, which Config.Validate never sees on the way to a boot.
// The hostless spelling is the interesting one: ":9091" is a port with no
// interface in front of it, and the tag has to accept it for a deployment to
// be able to write the loopback form at all.
func TestManagementAddressBindsThroughItsTag(t *testing.T) {
	for _, test := range []struct {
		addr string
		ok   bool
	}{
		{addr: "", ok: true},
		{addr: ":9091", ok: true},
		{addr: "127.0.0.1:9091", ok: true},
		{addr: "9091", ok: false},
		{addr: "127.0.0.1:not-a-port", ok: false},
	} {
		environment, err := config.NewEnvironment(map[string]any{
			"web": map[string]any{"management": map[string]any{"addr": test.addr}},
		}, "XBC_WEB_MANAGEMENT_ADDR_TEST_")
		require.NoError(t, err)

		var bound Config
		require.NoError(t, environment.Bind(ConfigPath, &bound))
		if test.ok {
			assert.NoErrorf(t, config.Validate(&bound, ConfigPath), "management.addr %q", test.addr)
			continue
		}
		assert.Errorf(t, config.Validate(&bound, ConfigPath), "management.addr %q", test.addr)
	}
}

func TestConfigRejectsMalformedManagementSettings(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"port without a host separator", func(c *Config) { c.Management.Addr = "9091" }},
		{"empty port", func(c *Config) { c.Management.Addr = "127.0.0.1:" }},
		{"padded address", func(c *Config) { c.Management.Addr = " 127.0.0.1:9091" }},
		{"remote address without allow_remote", func(c *Config) { c.Management.Addr = "0.0.0.0:9091" }},
		{"remote hostname without allow_remote", func(c *Config) { c.Management.Addr = "metrics.internal:9091" }},
		{"allow_remote without an address", func(c *Config) { c.Management.AllowRemote = true }},
		{"allow_remote without a certificate", func(c *Config) {
			c.Management.Addr = "0.0.0.0:9091"
			c.Management.AllowRemote = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.edit(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}
}

func TestConfigRejectsMalformedTLSSettings(t *testing.T) {
	signed := func(c *Config) {
		c.TLS.CertFile = "/etc/tls/tls.crt"
		c.TLS.KeyFile = "/etc/tls/tls.key"
	}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"certificate without a key", func(c *Config) { c.TLS.CertFile = "/etc/tls/tls.crt" }},
		{"key without a certificate", func(c *Config) { c.TLS.KeyFile = "/etc/tls/tls.key" }},
		{"padded certificate path", func(c *Config) { signed(c); c.TLS.CertFile = " /etc/tls/tls.crt" }},
		{"deprecated minimum version", func(c *Config) { signed(c); c.TLS.MinVersion = "1.1" }},
		{"unknown minimum version", func(c *Config) { signed(c); c.TLS.MinVersion = "tls1.2" }},
		{"unverified client auth mode", func(c *Config) { signed(c); c.TLS.ClientAuth = "request" }},
		{"client auth mode without a CA", func(c *Config) { signed(c); c.TLS.ClientAuth = "require_and_verify" }},
		{"CA without a verification mode", func(c *Config) { signed(c); c.TLS.ClientCAFile = "/etc/tls/ca.crt" }},
		{"insecure cipher suite", func(c *Config) { signed(c); c.TLS.CipherSuites = []string{"TLS_RSA_WITH_RC4_128_SHA"} }},
		{"unknown cipher suite", func(c *Config) { signed(c); c.TLS.CipherSuites = []string{"TLS_NOT_A_SUITE"} }},
		{"duplicate cipher suite", func(c *Config) {
			signed(c)
			c.TLS.CipherSuites = []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}
		}},
		{"negative reload interval", func(c *Config) { signed(c); c.TLS.ReloadInterval = -time.Second }},
		{"reload interval without a certificate", func(c *Config) { c.TLS.ReloadInterval = time.Minute }},
		{"client CA without a certificate", func(c *Config) { c.TLS.ClientCAFile = "/etc/tls/ca.crt"; c.TLS.ClientAuth = "require_and_verify" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.edit(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}

	cfg := DefaultConfig()
	signed(&cfg)
	cfg.TLS.MinVersion = "1.3"
	cfg.TLS.ClientAuth = "verify_if_given"
	cfg.TLS.ClientCAFile = "/etc/tls/ca.crt"
	cfg.TLS.CipherSuites = []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384"}
	cfg.TLS.ReloadInterval = 30 * time.Second
	assert.NoError(t, cfg.Validate(), "a fully declared TLS section is valid")
}
