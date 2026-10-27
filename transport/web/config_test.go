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
	assert.Zero(t, cfg.MaxRequestBodyBytesCeiling, "unset, so the ceiling follows max_request_body_bytes and no declaration can widen it")
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

// TestRequireRouteAuthorizerBindsFromTheWebSection pins the key that turns the
// missing-authorizer warning into a startup failure. The default stays false --
// covered by TestConfigDefaultsMatchDirectConstruction -- so an existing
// composition keeps booting; this field is the only place the opt-in is read.
func TestRequireRouteAuthorizerBindsFromTheWebSection(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"web": map[string]any{"security": map[string]any{"require_route_authorizer": true}},
	}, "XBC_WEB_REQUIRE_ROUTE_AUTHORIZER_TEST_")
	require.NoError(t, err)

	var bound Config
	require.NoError(t, environment.Bind(ConfigPath, &bound))
	require.NoError(t, config.Validate(&bound, ConfigPath))
	assert.True(t, bound.Security.RequireRouteAuthorizer)
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
		{"negative body limit ceiling", func(c *Config) { c.MaxRequestBodyBytesCeiling = -1 }},
		{"body limit ceiling below the default limit", func(c *Config) { c.MaxRequestBodyBytesCeiling = c.MaxRequestBodyBytes - 1 }},
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
				"addr":           "127.0.0.1:9091",
				"allow_remote":   false,
				"client_ca_file": "/etc/tls/management-ca.crt",
			},
			"tls": map[string]any{
				"cert_file":       "/etc/tls/tls.crt",
				"key_file":        "/etc/tls/tls.key",
				"http2":           true,
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
	assert.Equal(t, "/etc/tls/management-ca.crt", bound.Management.ClientCAFile)
	assert.Equal(t, "/etc/tls/tls.crt", bound.TLS.CertFile)
	assert.Equal(t, "/etc/tls/tls.key", bound.TLS.KeyFile)
	assert.True(t, bound.TLS.HTTP2)
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
	cfg.Management.ClientCAFile = "/etc/tls/management-ca.pem"
	assert.NoError(t, cfg.Validate(), "a remote management address with TLS and client certificates is the declared-remote case")
}

// TestManagementClientCAValidationMatrix walks every row of the management
// access matrix in .claude/specs/2026-10-20-web-management-remote-auth.md.
// The refusals that existed before management.client_ca_file are compared
// verbatim: they are the messages an operator already acts on, and a check
// inserted in front of one would silently replace the remedy with a different
// complaint. The rows this change adds are asserted on the setting each one
// has to name and the remedy it offers -- the outcome a remote management
// surface gets is a startup failure, so the message is the feature.
func TestManagementClientCAValidationMatrix(t *testing.T) {
	const (
		certFile = "/etc/tls/tls.crt"
		caFile   = "/etc/tls/management-ca.pem"
	)
	const (
		allowRemoteNeedsAddress = "web: management.allow_remote is set but management.addr is empty, so there is no management listener to permit"
		allowRemoteNeedsCert    = "web: management.allow_remote requires web.tls.cert_file\n" +
			"  → a remotely reachable management listener is an unauthenticated operator surface; the certificate is what keeps it off a cleartext network"
		remoteNeedsAllowRemote = "web: management.addr \"0.0.0.0:9091\" binds an address that is not loopback\n" +
			"  → the management listener carries no business authentication, so its access control is the bound address; set management.allow_remote: true (which requires web.tls.cert_file) to expose it, or leave the host out to bind loopback"
	)

	tests := []struct {
		name        string
		addr        string
		allowRemote bool
		certFile    string
		clientCA    string
		wantExact   string
		wantParts   []string
	}{
		{
			name: "default: no management listener",
		},
		{
			name:      "client CA without a management address",
			clientCA:  caFile,
			wantParts: []string{"management.client_ca_file", "management.addr", "no management listener to protect"},
		},
		{
			name:        "allow_remote without a management address",
			allowRemote: true,
			wantExact:   allowRemoteNeedsAddress,
		},
		{
			name:        "allow_remote and a client CA without a management address",
			allowRemote: true,
			clientCA:    caFile,
			wantExact:   allowRemoteNeedsAddress,
		},
		{
			name: "loopback without TLS",
			addr: "127.0.0.1:9091",
		},
		{
			name:     "loopback with serving TLS",
			addr:     "127.0.0.1:9091",
			certFile: certFile,
		},
		{
			name:     "loopback with serving TLS and client certificates",
			addr:     "127.0.0.1:9091",
			certFile: certFile,
			clientCA: caFile,
		},
		{
			name:      "loopback client CA without serving TLS",
			addr:      "127.0.0.1:9091",
			clientCA:  caFile,
			wantParts: []string{"management.client_ca_file", "web.tls.cert_file", "no in-process TLS to require a client certificate on"},
		},
		{
			// allow_remote is a permission, not an address: on a listener that
			// still binds loopback it is inert, and the existing row that
			// accepts it must keep accepting it.
			name:        "loopback allow_remote with serving TLS but no client CA",
			addr:        ":9091",
			allowRemote: true,
			certFile:    certFile,
		},
		{
			name:      "remote without allow_remote",
			addr:      "0.0.0.0:9091",
			wantExact: remoteNeedsAllowRemote,
		},
		{
			name:      "remote with TLS but without allow_remote",
			addr:      "0.0.0.0:9091",
			certFile:  certFile,
			clientCA:  caFile,
			wantExact: remoteNeedsAllowRemote,
		},
		{
			name:        "remote allow_remote without serving TLS",
			addr:        "0.0.0.0:9091",
			allowRemote: true,
			wantExact:   allowRemoteNeedsCert,
		},
		{
			// The serving certificate is refused before the client CA is
			// considered, so the operator fixes the listener that never
			// terminates TLS rather than a CA that would have nothing to
			// verify against.
			name:        "remote allow_remote with a client CA but without serving TLS",
			addr:        "0.0.0.0:9091",
			allowRemote: true,
			clientCA:    caFile,
			wantExact:   allowRemoteNeedsCert,
		},
		{
			name:        "remote allow_remote without a client CA",
			addr:        "0.0.0.0:9091",
			allowRemote: true,
			certFile:    certFile,
			wantParts:   []string{"management.allow_remote requires management.client_ca_file", "authenticate the operator", "loopback"},
		},
		{
			name:        "remote allow_remote with both files",
			addr:        "0.0.0.0:9091",
			allowRemote: true,
			certFile:    certFile,
			clientCA:    caFile,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Management.Addr = test.addr
			cfg.Management.AllowRemote = test.allowRemote
			cfg.Management.ClientCAFile = test.clientCA
			if test.certFile != "" {
				cfg.TLS = TLSConfig{CertFile: test.certFile, KeyFile: "/etc/tls/tls.key"}
			}

			err := cfg.Validate()
			if test.wantExact == "" && len(test.wantParts) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if test.wantExact != "" {
				assert.Equal(t, test.wantExact, err.Error(),
					"a refusal that already existed must keep its message, check order included")
			}
			for _, part := range test.wantParts {
				assert.Contains(t, err.Error(), part)
			}
		})
	}
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

// TestConfigGatesHTTP2OnTLSMaterial covers the three states web.tls.http2 can
// be in at validation time. Validation is I/O-free by contract, so fake paths
// are enough to accept the enabled case; what the key actually requires is the
// certificate pair being declared, not being readable.
func TestConfigGatesHTTP2OnTLSMaterial(t *testing.T) {
	enabled := DefaultConfig()
	enabled.TLS = TLSConfig{
		CertFile: "/etc/tls/tls.crt",
		KeyFile:  "/etc/tls/tls.key",
		HTTP2:    true,
	}
	assert.NoError(t, enabled.Validate(),
		"http2 with a declared certificate pair is valid; whether the files load is a startup question")

	disabled := DefaultConfig()
	assert.False(t, disabled.TLS.HTTP2, "HTTP/2 is opt-in: the default config must keep it off")
	disabled.TLS = TLSConfig{HTTP2: false}
	assert.NoError(t, disabled.Validate(), "an explicit false without TLS is the plain-HTTP deployment")

	withoutCertificate := DefaultConfig()
	withoutCertificate.TLS = TLSConfig{HTTP2: true}
	err := withoutCertificate.Validate()
	require.Error(t, err)
	assert.Equal(t,
		"web: tls.http2 requires tls.cert_file\n"+
			"  → HTTP/2 is offered through ALPN by the listener this process terminates; enable TLS with cert_file/key_file, or terminate HTTP/2 at the proxy and keep this listener on HTTP/1.1",
		err.Error(),
		"the refusal must name the key that is missing and the two ways to fix it")

	// A TLS 1.2 CBC suite is legal HTTP/1.1 and fatal HTTP/2: the standard
	// library's h2 server resets every connection that negotiated one, so a
	// configuration naming it would break h2 clients while HTTP/1.1 kept
	// working -- exactly the silent split this guard prevents.
	cbc := DefaultConfig()
	cbc.TLS = TLSConfig{
		CertFile:     "/etc/tls/tls.crt",
		KeyFile:      "/etc/tls/tls.key",
		HTTP2:        true,
		CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA"},
	}
	err = cbc.Validate()
	require.Error(t, err)
	assert.Equal(t,
		"web: tls.cipher_suites names \"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA\", which HTTP/2 prohibits (RFC 9113 Appendix A)\n"+
			"  → the standard library's HTTP/2 server resets every connection that negotiates this TLS 1.2 CBC suite; remove the suite, set tls.http2 to false, or raise tls.min_version to 1.3",
		err.Error(),
		"an h2 listener must refuse the CBC suite its own server rejects at the handshake")

	aead := DefaultConfig()
	aead.TLS = TLSConfig{
		CertFile:     "/etc/tls/tls.crt",
		KeyFile:      "/etc/tls/tls.key",
		HTTP2:        true,
		CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
	}
	assert.NoError(t, aead.Validate(), "an AEAD suite is what HTTP/2 requires, so it must stay valid")

	unreachable := DefaultConfig()
	unreachable.TLS = TLSConfig{
		CertFile:     "/etc/tls/tls.crt",
		KeyFile:      "/etc/tls/tls.key",
		HTTP2:        true,
		MinVersion:   "1.3",
		CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA"},
	}
	assert.NoError(t, unreachable.Validate(),
		"min_version 1.3 makes every TLS 1.2 suite unreachable, so the key has no effect and is not refused")
}

// TestMaxRequestBodyBytesCeilingBindsFromTheWebSection pins the configuration key
// itself. The field is the only thing the ceiling can be read from, so a rename
// that silently stopped binding would leave a deployment that raised it serving
// every upload route at the default limit -- a failure with no error attached.
func TestMaxRequestBodyBytesCeilingBindsFromTheWebSection(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"web": map[string]any{"max_request_body_bytes_ceiling": 200 << 20},
	}, "XBC_WEB_BODY_CEILING_TEST_")
	require.NoError(t, err)

	var bound Config
	require.NoError(t, environment.Bind(ConfigPath, &bound))
	require.NoError(t, config.Validate(&bound, ConfigPath))
	require.NoError(t, bound.Validate())
	assert.Equal(t, int64(200<<20), bound.MaxRequestBodyBytesCeiling)
	assert.Equal(t, int64(10<<20), bound.MaxRequestBodyBytes, "the default limit keeps its meaning and its default")
}

// TestMaxRequestBodyBytesCeilingValidation pins the relationship between the two
// keys. Unset is valid and means "follow max_request_body_bytes"; equal and
// larger are valid; anything below the default limit is refused with an error
// that names both keys, because lowering either one repairs it and the operator
// should not have to guess which.
func TestMaxRequestBodyBytesCeilingValidation(t *testing.T) {
	valid := []struct {
		name    string
		limit   int64
		ceiling int64
	}{
		{"unset", 10 << 20, 0},
		{"equal to the default limit", 10 << 20, 10 << 20},
		{"above the default limit", 10 << 20, 200 << 20},
		{"unset while the default limit is raised", 256 << 20, 0},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MaxRequestBodyBytes = test.limit
			cfg.MaxRequestBodyBytesCeiling = test.ceiling
			assert.NoError(t, cfg.Validate())
		})
	}

	t.Run("below the default limit names both keys and both values", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRequestBodyBytes = 10 << 20
		cfg.MaxRequestBodyBytesCeiling = 5 << 20
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_request_body_bytes_ceiling (5242880)")
		assert.Contains(t, err.Error(), "max_request_body_bytes (10485760)")
		assert.Contains(t, err.Error(), "raise max_request_body_bytes_ceiling")
	})
	t.Run("negative says to leave it unset", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRequestBodyBytesCeiling = -1
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_request_body_bytes_ceiling")
		assert.Contains(t, err.Error(), "leave it unset")
	})
}

// TestNormalizeConfigResolvesTheUnsetBodyCeiling pins the backward-compatible
// default at the point a Server reads it. An unset ceiling becomes the default
// limit actually in force -- including a default limit that was itself unset and
// defaulted a line earlier, which is the ordering a careless edit gets wrong --
// and a ceiling that was set is left alone.
func TestNormalizeConfigResolvesTheUnsetBodyCeiling(t *testing.T) {
	t.Run("unset follows the configured default limit", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRequestBodyBytes = 32 << 20
		resolved, err := normalizeConfig(cfg)
		require.NoError(t, err)
		assert.Equal(t, int64(32<<20), resolved.MaxRequestBodyBytesCeiling)
	})
	t.Run("unset follows a default limit that was itself unset", func(t *testing.T) {
		resolved, err := normalizeConfig(Config{})
		require.NoError(t, err)
		assert.Equal(t, int64(10<<20), resolved.MaxRequestBodyBytes)
		assert.Equal(t, int64(10<<20), resolved.MaxRequestBodyBytesCeiling,
			"the ceiling must follow the resolved default, not the zero the caller left")
	})
	t.Run("a configured ceiling is kept", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRequestBodyBytesCeiling = 200 << 20
		resolved, err := normalizeConfig(cfg)
		require.NoError(t, err)
		assert.Equal(t, int64(200<<20), resolved.MaxRequestBodyBytesCeiling)
	})
	t.Run("a ceiling below the default limit is refused", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRequestBodyBytesCeiling = 1 << 20
		_, err := normalizeConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_request_body_bytes_ceiling")
	})
}
