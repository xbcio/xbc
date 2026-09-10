package jwt

import (
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid defaults"},
		{name: "secret too short", mutate: func(c *Config) { c.Secret = strings.Repeat("x", 31) }, wantErr: "at least 32 bytes"},
		{name: "none signing algorithm", mutate: func(c *Config) { c.Algorithm = "none" }, wantErr: "unsupported signing algorithm"},
		{name: "none in allowlist", mutate: func(c *Config) { c.Algorithms = []string{"HS256", "none"} }, wantErr: "unsupported algorithm"},
		{name: "asymmetric confusion", mutate: func(c *Config) { c.Algorithms = []string{"RS256"} }, wantErr: "unsupported algorithm"},
		{name: "signing algorithm missing from allowlist", mutate: func(c *Config) { c.Algorithms = []string{"HS512"} }, wantErr: "not in the algorithms allowlist"},
		{name: "empty allowlist", mutate: func(c *Config) { c.Algorithms = []string{} }, wantErr: "allowlist cannot be empty"},
		{name: "negative leeway", mutate: func(c *Config) { c.Leeway = -time.Second }, wantErr: "leeway cannot be negative"},
		{name: "negative expiry", mutate: func(c *Config) { c.Expire = -time.Second }, wantErr: "expire must be greater than zero"},
		{name: "bad header", mutate: func(c *Config) { c.Header = "X Auth" }, wantErr: "valid HTTP header"},
		{name: "bad scheme", mutate: func(c *Config) { c.Scheme = "My Scheme" }, wantErr: "valid HTTP authentication scheme"},
		{name: "empty audience", mutate: func(c *Config) { c.Audience = []string{"service", " "} }, wantErr: "audience values cannot be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Secret = testSecret
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
			if strings.Contains(strings.TrimSpace(errorText(err)), testSecret) {
				t.Fatal("validation error leaked secret")
			}
		})
	}
}

// TestSchemaValidationDoesNotEchoTheSecret guards the mask:"true" marker on
// Secret. Config.Validate is written to never include the secret, but the
// struct's own min=32 rule is enforced by config.Validate, which renders the
// offending value -- so the marker, not the hand-written method, is what keeps
// a short signing key out of the startup error.
func TestSchemaValidationDoesNotEchoTheSecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Secret = "hunter2-live-signing-key"

	err := config.Validate(&cfg, "plugins.jwt")
	if err == nil {
		t.Fatal("config.Validate() = nil, want a min=32 violation")
	}
	if strings.Contains(err.Error(), cfg.Secret) {
		t.Fatalf("validation error leaked the signing key: %v", err)
	}
	if !strings.Contains(err.Error(), "plugins.jwt.secret") {
		t.Fatalf("validation error must still name the path to edit: %v", err)
	}
}

func TestAllSupportedAlgorithmsCanSignAndVerify(t *testing.T) {
	for _, algorithm := range []string{"HS256", "HS384", "HS512"} {
		t.Run(algorithm, func(t *testing.T) {
			p := configuredPlugin(t, func(cfg *Config) {
				cfg.Algorithm = algorithm
				cfg.Algorithms = []string{algorithm}
			})
			token, err := p.Sign("subject", Claims{"role": "admin"})
			if err != nil {
				t.Fatalf("Sign() error = %v", err)
			}
			claims, err := p.compiled.verify(token)
			if err != nil {
				t.Fatalf("verify() error = %v", err)
			}
			if claims["role"] != "admin" {
				t.Fatalf("role = %#v, want admin", claims["role"])
			}
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
