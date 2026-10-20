package csrf

import (
	"strings"
	"testing"

	"github.com/xbcio/xbc/config"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	env, err := config.NewEnvironment(nil, "XBC_CSRF_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := env.Bind("plugins.csrf", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(&cfg, "plugins.csrf"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if cfg.Mode != "origin" {
		t.Fatalf("Mode = %q, want origin", cfg.Mode)
	}
	if cfg.Header != "X-CSRF-Token" {
		t.Fatalf("Header = %q, want X-CSRF-Token", cfg.Header)
	}
	if cfg.SessionCookie != "xbc_session" {
		t.Fatalf("SessionCookie = %q, want xbc_session", cfg.SessionCookie)
	}
	if cfg.Secret != "" {
		t.Fatalf("Secret = %q, want empty", cfg.Secret)
	}
	if len(cfg.AllowOrigins) != 0 || len(cfg.ExcludePaths) != 0 || len(cfg.ExcludeRoutes) != 0 {
		t.Fatalf("list defaults = %v/%v/%v, want empty", cfg.AllowOrigins, cfg.ExcludePaths, cfg.ExcludeRoutes)
	}
	if got := DefaultConfig(); got.Mode != cfg.Mode || got.Header != cfg.Header || got.SessionCookie != cfg.SessionCookie {
		t.Fatalf("DefaultConfig() = %#v, want matching defaults", got)
	}
}

func TestConfigAcceptsValidValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"origin mode", func(c *Config) {}},
		{"explicit origin mode", func(c *Config) { c.Mode = "origin" }},
		{"token mode with 32-byte secret", func(c *Config) {
			c.Mode = "token"
			c.Secret = strings.Repeat("s", 32)
		}},
		{"origin mode with a secret", func(c *Config) { c.Secret = strings.Repeat("s", 32) }},
		{"trailing slash on an allowed origin", func(c *Config) { c.AllowOrigins = []string{"https://app.example.com/"} }},
		{"allowed origins with ports", func(c *Config) { c.AllowOrigins = []string{"http://127.0.0.1:8080", "https://app.example.com:8443"} }},
		{"valid exclusion patterns", func(c *Config) {
			c.ExcludePaths = []string{"/healthz", "/auth/*/callback", "/items/:id"}
			c.ExcludeRoutes = []string{"health.live", "oidc.callback"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.edit(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestConfigRejectsUnsafeOrMalformedValues(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		edit      func(*Config)
		wantInErr string
	}{
		{"unknown mode", "mode", func(c *Config) { c.Mode = "strict" }, "mode"},
		{"wildcard origin", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"*"} }, "wildcard"},
		{"wildcard pattern", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"https://*.example.com"} }, "wildcard"},
		{"relative origin", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"app.example.com"} }, "absolute"},
		{"non-http scheme", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"ftp://app.example.com"} }, "absolute"},
		{"origin path", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"https://app.example.com/path"} }, "absolute"},
		{"origin query", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"https://app.example.com?x=1"} }, "absolute"},
		{"origin fragment", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"https://app.example.com#fragment"} }, "absolute"},
		{"origin userinfo", "allow_origins", func(c *Config) { c.AllowOrigins = []string{"https://user:pass@app.example.com"} }, "absolute"},
		{"empty origin", "allow_origins", func(c *Config) { c.AllowOrigins = []string{""} }, "allow_origins"},
		{"invalid header name", "header", func(c *Config) { c.Header = "X Bad Header" }, "header"},
		{"empty header name", "header", func(c *Config) { c.Header = "" }, "header"},
		{"invalid cookie name", "session_cookie", func(c *Config) { c.SessionCookie = "bad cookie" }, "session_cookie"},
		{"empty cookie name", "session_cookie", func(c *Config) { c.SessionCookie = "" }, "session_cookie"},
		{"empty exclude path", "exclude_paths", func(c *Config) { c.ExcludePaths = []string{""} }, "exclude_paths"},
		{"malformed exclude path", "exclude_paths", func(c *Config) { c.ExcludePaths = []string{"/items/["} }, "exclude_paths"},
		{"empty exclude route", "exclude_routes", func(c *Config) { c.ExcludeRoutes = []string{""} }, "exclude_routes"},
		{"short secret", "secret", func(c *Config) { c.Secret = strings.Repeat("s", 31) }, "secret"},
		{"token mode without secret", "secret", func(c *Config) { c.Mode = "token" }, "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.edit(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Validate() error = %q, want it to name field %q", err, tt.field)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.wantInErr)
			}
		})
	}
}

func TestCompilePolicyNormalizesAllowedOrigins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://app.example.com/", "http://localhost:8080"}
	compiled, err := compilePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://app.example.com", "http://localhost:8080"} {
		if _, ok := compiled.allowOrigins[origin]; !ok {
			t.Fatalf("allowOrigins = %v, want normalized entry %q", compiled.allowOrigins, origin)
		}
	}
}

func TestCompilePolicyCopiesExclusionLists(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ExcludePaths = []string{"/healthz"}
	compiled, err := compilePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExcludePaths[0] = "/mutated"
	if compiled.excludePaths[0] != "/healthz" {
		t.Fatalf("compiled excludePaths aliased the configuration: %v", compiled.excludePaths)
	}
}
