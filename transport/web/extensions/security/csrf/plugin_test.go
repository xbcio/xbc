package csrf

import (
	"strings"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestDefinitionAndMiddlewareMetadata(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	order := middleware.Order()
	if order.Phase != web.PhaseSecurity || len(order.After) != 0 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if p.policy.mode != modeOrigin {
		t.Fatalf("mode = %v, want origin", p.policy.mode)
	}
	if p.policy.header != defaultHeader || p.policy.sessionCookie != defaultSessionCookie {
		t.Fatalf("header/session cookie = %q/%q, want %q/%q", p.policy.header, p.policy.sessionCookie, defaultHeader, defaultSessionCookie)
	}
}

func TestConfiguredConstructionRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"token mode without a secret", func(c *Config) { c.Mode = "token" }},
		{"short secret", func(c *Config) { c.Secret = strings.Repeat("s", 31) }},
		{"wildcard origin", func(c *Config) { c.AllowOrigins = []string{"*"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.edit(&cfg)
			if _, err := prepareConfig(cfg); err == nil {
				t.Fatal("prepareConfig() succeeded, want error")
			}
			if _, err := New(cfg); err == nil {
				t.Fatal("New() succeeded, want error")
			}
		})
	}
}
