package oidc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// validConfig is a configuration that passes every static rule. Tests mutate
// one field at a time from a copy.
func validConfig() Config {
	return Config{
		Name:         "test",
		Issuer:       "https://idp.example.com",
		ClientID:     "xbc-client",
		ClientSecret: outbound.Secret("client-secret"),
		RedirectURI:  "https://admin.example.com/auth/test/callback",
	}
}

func TestConfigValidationRejectsUnsafeSettings(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing name", func(c *Config) { c.Name = "" }},
		{"name with space", func(c *Config) { c.Name = "bad name" }},
		{"name too long", func(c *Config) { c.Name = strings.Repeat("a", 65) }},
		{"name starting with dash", func(c *Config) { c.Name = "-bad" }},
		{"missing issuer", func(c *Config) { c.Issuer = "" }},
		{"issuer not absolute", func(c *Config) { c.Issuer = "/issuer" }},
		{"issuer with query", func(c *Config) { c.Issuer = "https://idp.example.com?tenant=1" }},
		{"issuer with fragment", func(c *Config) { c.Issuer = "https://idp.example.com#fragment" }},
		{"issuer with userinfo", func(c *Config) { c.Issuer = "https://user:pass@idp.example.com" }},
		{"issuer with non-HTTP scheme", func(c *Config) { c.Issuer = "ftp://idp.example.com" }},
		{"plain HTTP issuer without the development switch", func(c *Config) { c.Issuer = "http://idp.example.com" }},
		{"missing client id", func(c *Config) { c.ClientID = "" }},
		{"client id with whitespace", func(c *Config) { c.ClientID = "xbc client" }},
		{"missing client secret", func(c *Config) { c.ClientSecret = "" }},
		{"missing redirect URI", func(c *Config) { c.RedirectURI = "" }},
		{"relative redirect URI", func(c *Config) { c.RedirectURI = "/callback" }},
		{"redirect URI wildcard", func(c *Config) { c.RedirectURI = "https://admin.example.com/*" }},
		{"redirect URI with fragment", func(c *Config) { c.RedirectURI = "https://admin.example.com/callback#fragment" }},
		{"redirect URI with userinfo", func(c *Config) { c.RedirectURI = "https://user:pass@admin.example.com/callback" }},
		{"plain HTTP redirect URI without the development switch", func(c *Config) { c.RedirectURI = "http://admin.example.com/callback" }},
		{"scopes without openid", func(c *Config) { c.Scopes = []string{"profile", "email"} }},
		{"scope with a space", func(c *Config) { c.Scopes = []string{"openid", "profile email"} }},
		{"unsupported token endpoint auth method", func(c *Config) { c.TokenEndpointAuthMethod = "private_key_jwt" }},
		{"algorithm none", func(c *Config) { c.AllowedAlgorithms = []string{"none"} }},
		{"algorithm HS256", func(c *Config) { c.AllowedAlgorithms = []string{"HS256"} }},
		{"unknown algorithm", func(c *Config) { c.AllowedAlgorithms = []string{"ES256K"} }},
		{"negative leeway", func(c *Config) { c.Leeway = -time.Second }},
		{"leeway above the hard cap", func(c *Config) { c.Leeway = 61 * time.Second }},
		{"negative JWKS cache TTL", func(c *Config) { c.JWKSCacheTTL = -time.Second }},
		{"negative JWKS refresh interval", func(c *Config) { c.JWKSMinRefreshInterval = -time.Second }},
		{"cache TTL shorter than the refresh interval", func(c *Config) {
			c.JWKSCacheTTL = time.Second
			c.JWKSMinRefreshInterval = time.Minute
		}},
		{"negative outbound timeout", func(c *Config) { c.Policy.TotalTimeout = -time.Second }},
		{"negative outbound body limit", func(c *Config) { c.Policy.MaxBodyBytes = -1 }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			config := validConfig()
			testCase.mutate(&config)
			_, err := New(config)
			requireErrorIs(t, err, ErrInvalidConfig, testCase.name)
		})
	}
}

func TestConfigAcceptsDevelopmentHTTP(t *testing.T) {
	config := validConfig()
	config.Issuer = "http://127.0.0.1:8080"
	config.RedirectURI = "http://127.0.0.1:8080/auth/test/callback"
	config.Policy.AllowHTTP = true
	config.Policy.AllowPrivate = true
	provider, err := New(config)
	requireNoError(t, err, "HTTP development issuer and callback")
	if provider.Name() != "test" {
		t.Fatalf("Name() = %q, want test", provider.Name())
	}
}

func TestConfigAcceptsEveryAllowedAlgorithm(t *testing.T) {
	for _, algorithm := range []string{
		"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512",
	} {
		config := validConfig()
		config.AllowedAlgorithms = []string{algorithm}
		if _, err := New(config); err != nil {
			t.Errorf("algorithm %s was rejected: %v", algorithm, err)
		}
	}
}

func TestNormalizeConfigDefaults(t *testing.T) {
	normalized, err := normalizeConfig(validConfig())
	requireNoError(t, err, "normalize default config")

	if normalized.name != "test" || normalized.issuer != "https://idp.example.com" {
		t.Fatalf("identity fields were not preserved: %+v", normalized)
	}
	if got, want := strings.Join(normalized.scopes, " "), "openid"; got != want {
		t.Fatalf("default scopes = %q, want %q", got, want)
	}
	if normalized.scope != "openid" {
		t.Fatalf("scope string = %q, want \"openid\"", normalized.scope)
	}
	if normalized.authMethod != ClientSecretBasic {
		t.Fatalf("default auth method = %q, want %q", normalized.authMethod, ClientSecretBasic)
	}
	if got, want := strings.Join(normalized.algorithms, ","), "RS256,ES256"; got != want {
		t.Fatalf("default algorithms = %q, want %q", got, want)
	}
	if normalized.leeway != defaultLeeway {
		t.Fatalf("default leeway = %s, want %s", normalized.leeway, defaultLeeway)
	}
	if normalized.jwksTTL != defaultJWKSCacheTTL {
		t.Fatalf("default JWKS TTL = %s, want %s", normalized.jwksTTL, defaultJWKSCacheTTL)
	}
	if normalized.jwksRefresh != defaultJWKSMinRefreshInterval {
		t.Fatalf("default JWKS refresh = %s, want %s", normalized.jwksRefresh, defaultJWKSMinRefreshInterval)
	}
	if normalized.now == nil {
		t.Fatal("default clock was not installed")
	}
}

func TestConfigRedactsClientSecret(t *testing.T) {
	config := validConfig()
	const secret = "client-secret"
	views := map[string]string{
		"String":      config.String(),
		"%v":          fmt.Sprintf("%v", config),
		"%+v":         fmt.Sprintf("%+v", config),
		"%#v":         fmt.Sprintf("%#v", config),
		"%s":          fmt.Sprintf("%s", config),
		"nested view": fmt.Sprintf("%v", struct{ Config Config }{config}),
	}
	for name, view := range views {
		if strings.Contains(view, secret) {
			t.Fatalf("%s rendered the client secret: %s", name, view)
		}
		if !strings.Contains(view, "[REDACTED]") {
			t.Fatalf("%s did not render the redaction marker: %s", name, view)
		}
	}
}

func TestConfigValidateMatchesNew(t *testing.T) {
	config := validConfig()
	requireNoError(t, config.Validate(), "valid config Validate")

	invalid := validConfig()
	invalid.Leeway = 2 * time.Minute
	requireErrorIs(t, invalid.Validate(), ErrInvalidConfig, "invalid config Validate")
}
