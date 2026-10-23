package oauth2

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// validConfig is a static configuration that needs no network.
func validConfig() Config {
	return Config{
		Name:                  "corp",
		Issuer:                "https://idp.example.com",
		AuthorizationEndpoint: "https://idp.example.com/authorize",
		TokenEndpoint:         "https://idp.example.com/token",
		UserInfoEndpoint:      "https://idp.example.com/userinfo",
		ClientID:              "xbc-client",
		ClientSecret:          outbound.Secret("client-secret"),
		RedirectURI:           "https://admin.example.com/auth/oauth2/corp/callback",
		Scopes:                []string{"profile", "email"},
		SubjectField:          "sub",
		ClaimFields:           map[string]string{"email": "email"},
	}
}

func TestNewAcceptsTheDocumentedConfiguration(t *testing.T) {
	provider, err := New(validConfig())
	requireNoError(t, err, "New(validConfig)")
	if provider == nil {
		t.Fatal("New returned a nil provider")
	}
	if got := provider.Name(); got != "corp" {
		t.Fatalf("Name() = %q, want corp", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	normalized, err := normalizeConfig(validConfig())
	requireNoError(t, err, "normalizeConfig")
	if normalized.authMethod != ClientSecretBasic {
		t.Fatalf("default auth method = %q, want %q", normalized.authMethod, ClientSecretBasic)
	}
	if normalized.now == nil {
		t.Fatal("default clock was not installed")
	}

	empty := validConfig()
	empty.Scopes = nil
	normalized, err = normalizeConfig(empty)
	requireNoError(t, err, "normalizeConfig without scopes")
	if normalized.scopes != nil || normalized.scope != "" {
		t.Fatalf("empty scopes normalized to %v / %q, want no scope parameter", normalized.scopes, normalized.scope)
	}
}

func TestConfigDeduplicatesScopes(t *testing.T) {
	config := validConfig()
	config.Scopes = []string{"profile", "email", "profile"}
	normalized, err := normalizeConfig(config)
	requireNoError(t, err, "normalizeConfig")
	if got := normalized.scope; got != "profile email" {
		t.Fatalf("scope = %q, want %q", got, "profile email")
	}
}

func TestNewRejectsInvalidConfigurations(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty name":                           func(c *Config) { c.Name = "" },
		"invalid name":                         func(c *Config) { c.Name = "bad name" },
		"name starting with underscore":        func(c *Config) { c.Name = "_corp" },
		"empty issuer":                         func(c *Config) { c.Issuer = "" },
		"relative issuer":                      func(c *Config) { c.Issuer = "idp.example.com" },
		"http issuer without allow":            func(c *Config) { c.Issuer = "http://idp.example.com" },
		"issuer with query":                    func(c *Config) { c.Issuer = "https://idp.example.com?tenant=1" },
		"issuer with userinfo":                 func(c *Config) { c.Issuer = "https://user:pass@idp.example.com" },
		"empty authorization endpoint":         func(c *Config) { c.AuthorizationEndpoint = "" },
		"relative authorization endpoint":      func(c *Config) { c.AuthorizationEndpoint = "/authorize" },
		"http token endpoint without allow":    func(c *Config) { c.TokenEndpoint = "http://idp.example.com/token" },
		"http userinfo endpoint without allow": func(c *Config) { c.UserInfoEndpoint = "http://idp.example.com/userinfo" },
		"token endpoint with query":            func(c *Config) { c.TokenEndpoint = "https://idp.example.com/token?x=1" },
		"userinfo endpoint with fragment":      func(c *Config) { c.UserInfoEndpoint = "https://idp.example.com/userinfo#x" },
		"userinfo endpoint with userinfo":      func(c *Config) { c.UserInfoEndpoint = "https://user:pass@idp.example.com/userinfo" },
		"empty client id":                      func(c *Config) { c.ClientID = "" },
		"client id with space":                 func(c *Config) { c.ClientID = "bad id" },
		"empty client secret":                  func(c *Config) { c.ClientSecret = "" },
		"wildcard redirect uri":                func(c *Config) { c.RedirectURI = "https://admin.example.com/*" },
		"http redirect without allow":          func(c *Config) { c.RedirectURI = "http://admin.example.com/callback" },
		"scope with space":                     func(c *Config) { c.Scopes = []string{"profile email"} },
		"unsupported auth method":              func(c *Config) { c.TokenEndpointAuthMethod = "private_key_jwt" },
		"empty subject field":                  func(c *Config) { c.SubjectField = "" },
		"subject path with empty segment":      func(c *Config) { c.SubjectField = "account..id" },
		"claim name with space":                func(c *Config) { c.ClaimFields = map[string]string{"bad name": "email"} },
		"claim path with empty segment":        func(c *Config) { c.ClaimFields = map[string]string{"email": "profile."} },
		"negative dial timeout":                func(c *Config) { c.Policy.DialTimeout = -time.Second },
		"negative body limit":                  func(c *Config) { c.Policy.MaxBodyBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := validConfig()
			mutate(&config)
			_, err := New(config)
			requireErrorIs(t, err, ErrInvalidConfig, "New(invalid config)")
		})
	}
}

func TestConfigAllowsHTTPThroughTheDevelopmentSwitch(t *testing.T) {
	config := validConfig()
	config.Issuer = "http://idp.example.com"
	config.AuthorizationEndpoint = "http://idp.example.com/authorize"
	config.TokenEndpoint = "http://idp.example.com/token"
	config.UserInfoEndpoint = "http://idp.example.com/userinfo"
	config.RedirectURI = "http://admin.example.com/callback"
	config.Policy.AllowHTTP = true
	_, err := New(config)
	requireNoError(t, err, "New with AllowHTTP")
}

func TestConfigValidateMatchesNew(t *testing.T) {
	config := validConfig()
	requireNoError(t, config.Validate(), "valid config Validate")

	invalid := validConfig()
	invalid.SubjectField = ""
	requireErrorIs(t, invalid.Validate(), ErrInvalidConfig, "invalid config Validate")
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

func TestProviderRendersWithTheClientSecretRedacted(t *testing.T) {
	provider, err := New(validConfig())
	requireNoError(t, err, "New")
	const secret = "client-secret"

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, provider)
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s rendered the client secret: %s", format, rendered)
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("%s did not mark the client secret as redacted: %s", format, rendered)
		}

		// A dereferenced Provider value must redact too: fmt only dispatches
		// methods from a struct's exported fields, so a Provider value without
		// its own formatter would print the embedded settings.
		rendered = fmt.Sprintf(format, *provider)
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s on a Provider value rendered the client secret: %s", format, rendered)
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("%s on a Provider value did not mark the client secret as redacted: %s", format, rendered)
		}
	}
}
