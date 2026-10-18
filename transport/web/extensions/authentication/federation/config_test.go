package federation

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigIsSafe(t *testing.T) {
	cfg := DefaultConfig()
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}
	if normalized.backend != BackendRedis {
		t.Fatalf("backend = %q, want the multi-replica default %q", normalized.backend, BackendRedis)
	}
	if normalized.transactionTTL != MaxTransactionTTL {
		t.Fatalf("transaction TTL = %s, want %s", normalized.transactionTTL, MaxTransactionTTL)
	}
	if !normalized.cookieSecure || normalized.cookieSameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie settings = secure:%v sameSite:%v, want secure lax", normalized.cookieSecure, normalized.cookieSameSite)
	}
	if normalized.cookieName != defaultCookieName || normalized.cookiePath != "/" {
		t.Fatalf("cookie = %q path %q", normalized.cookieName, normalized.cookiePath)
	}
	if normalized.failurePath != defaultFailurePath {
		t.Fatalf("failure path = %q, want %q", normalized.failurePath, defaultFailurePath)
	}
	if normalized.returnTo.fallback != "/" {
		t.Fatalf("fallback = %q, want /", normalized.returnTo.fallback)
	}
	if len(normalized.returnTo.origins) != 0 {
		t.Fatalf("default origins = %v, want none", normalized.returnTo.origins)
	}
}

func TestConfigNormalizationAppliesDefaults(t *testing.T) {
	normalized, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatalf("normalizeConfig(Config{}) error = %v", err)
	}
	if normalized.backend != defaultBackend || normalized.redisInstance != "default" {
		t.Fatalf("backend = %q instance = %q", normalized.backend, normalized.redisInstance)
	}
	if normalized.redisPrefix != defaultRedisPrefix {
		t.Fatalf("redis prefix = %q", normalized.redisPrefix)
	}
	if normalized.transactionTTL != MaxTransactionTTL {
		t.Fatalf("ttl = %s", normalized.transactionTTL)
	}
	if normalized.cookieName != defaultCookieName || normalized.cookiePath != "/" || normalized.cookieSameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v", normalized)
	}
	if normalized.failurePath != defaultFailurePath || normalized.returnTo.fallback != defaultReturnTo {
		t.Fatalf("landing = %q fallback = %q", normalized.failurePath, normalized.returnTo.fallback)
	}
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"unknown backend":            func(c *Config) { c.Backend = "memory-cache" },
		"zero ttl":                   func(c *Config) { c.TransactionTTL = -time.Second },
		"ttl above the hard bound":   func(c *Config) { c.TransactionTTL = MaxTransactionTTL + time.Second },
		"invalid cookie name":        func(c *Config) { c.CookieName = "bad name" },
		"invalid cookie path":        func(c *Config) { c.CookiePath = "relative" },
		"cookie path with semicolon": func(c *Config) { c.CookiePath = "/;Path=/" },
		"cookie domain with space":   func(c *Config) { c.CookieDomain = "evil domain" },
		"unknown same site":          func(c *Config) { c.CookieSameSite = "sometimes" },
		"same site none insecurely":  func(c *Config) { c.CookieSameSite = "none"; c.CookieSecure = false },
		"host cookie without secure": func(c *Config) { c.CookieName = "__Host-federation"; c.CookieSecure = false },
		"host cookie with domain":    func(c *Config) { c.CookieName = "__Host-federation"; c.CookieDomain = "example.com" },
		"secure cookie insecurely":   func(c *Config) { c.CookieName = "__Secure-federation"; c.CookieSecure = false },
		"absolute failure path":      func(c *Config) { c.FailurePath = "https://evil.example/failed" },
		"scheme-relative failure":    func(c *Config) { c.FailurePath = "//evil.example/failed" },
		"backslash failure path":     func(c *Config) { c.FailurePath = `/failed\..\evil` },
		"control characters in path": func(c *Config) { c.FailurePath = "/failed\r\nLocation: https://evil.example" },
		"backslash default landing":  func(c *Config) { c.DefaultReturnTo = `/home\..\evil` },
		"empty redis prefix":         func(c *Config) { c.Backend = BackendRedis; c.RedisPrefix = " " },
		"invalid redis instance":     func(c *Config) { c.RedisInstance = "bad instance" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			mutate(&cfg)
			if _, err := normalizeConfig(cfg); err == nil {
				t.Fatalf("normalizeConfig accepted %+v", cfg)
			}
		})
	}
}

func TestConfigAcceptsAllowlistedReturnOrigins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedReturnOrigins = []string{"https://APP.example.com:443/"}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig error = %v", err)
	}
	if normalized.returnTo.origins["https://app.example.com"] != true {
		t.Fatalf("origins = %v", normalized.returnTo.origins)
	}
	// The default port normalizes away and the host lowercases, so the two
	// spellings below are the same origin and the duplicate is rejected.
	cfg.AllowedReturnOrigins = []string{"https://app.example.com", "https://APP.example.com:443"}
	if _, err := normalizeConfig(cfg); err == nil {
		t.Fatal("normalizeConfig accepted a duplicated origin")
	}
}

func TestConfigRejectsUnsafeReturnOrigins(t *testing.T) {
	for name, origin := range map[string]string{
		"plain http":        "http://app.example.com",
		"relative":          "/app",
		"no host":           "https://",
		"carries a path":    "https://app.example.com/login",
		"carries a query":   "https://app.example.com?x=1",
		"carries userinfo":  "https://user@app.example.com",
		"carries backslash": `https://app.example.com\@evil.example`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AllowedReturnOrigins = []string{origin}
			if _, err := normalizeConfig(cfg); err == nil {
				t.Fatalf("normalizeConfig accepted origin %q", origin)
			}
		})
	}
}

func TestConfigAcceptsAllowlistedAbsoluteDefaultLanding(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowedReturnOrigins = []string{"https://app.example.com"}
	cfg.DefaultReturnTo = "https://app.example.com/after-login"
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig error = %v", err)
	}
	if normalized.returnTo.fallback != "https://app.example.com/after-login" {
		t.Fatalf("fallback = %q", normalized.returnTo.fallback)
	}

	cfg.DefaultReturnTo = "https://evil.example/after-login"
	if _, err := normalizeConfig(cfg); err == nil {
		t.Fatal("normalizeConfig accepted an unallowlisted default landing")
	}
}

func TestConfigValidationErrorMessageNamesTheSetting(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTTL = MaxTransactionTTL * 2
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "transaction_ttl") {
		t.Fatalf("Validate() error = %v, want a transaction_ttl diagnostic", err)
	}
}
