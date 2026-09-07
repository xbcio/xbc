package cors

import (
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	env, err := config.NewEnvironment(nil, "XBC_CORS_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := env.Bind("plugins.cors", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(&cfg, "plugins.cors"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	want := DefaultConfig()
	if len(cfg.AllowOrigins) != 1 || cfg.AllowOrigins[0] != "*" {
		t.Fatalf("AllowOrigins = %v, want [*]", cfg.AllowOrigins)
	}
	if len(cfg.AllowMethods) != len(want.AllowMethods) {
		t.Fatalf("AllowMethods = %v, want %v", cfg.AllowMethods, want.AllowMethods)
	}
	if cfg.MaxAge != 12*time.Hour {
		t.Fatalf("MaxAge = %s, want 12h", cfg.MaxAge)
	}
}

func TestConfigRejectsUnsafeOrMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"wildcard credentials", func(c *Config) { c.AllowCredentials = true }},
		{"wildcard expose with credentials", func(c *Config) {
			c.AllowOrigins = []string{"https://example.com"}
			c.AllowCredentials = true
			c.ExposeHeaders = []string{"*"}
		}},
		{"mixed wildcard", func(c *Config) { c.AllowOrigins = []string{"*", "https://example.com"} }},
		{"origin path", func(c *Config) { c.AllowOrigins = []string{"https://example.com/path"} }},
		{"unsupported origin pattern", func(c *Config) { c.AllowOrigins = []string{"https://*.example.com"} }},
		{"mixed method wildcard", func(c *Config) { c.AllowMethods = []string{"*", "GET"} }},
		{"invalid method", func(c *Config) { c.AllowMethods = []string{"NOT A METHOD"} }},
		{"invalid header", func(c *Config) { c.AllowHeaders = []string{"X-Good", "X-Bad\nInjected"} }},
		{"negative max age", func(c *Config) { c.MaxAge = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}
}
