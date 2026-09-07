package session

import (
	"testing"
	"time"
)

func TestConfigRejectsUnsafeCookieAndLifetimeSettings(t *testing.T) {
	base := DefaultConfig()
	cases := map[string]func(*Config){
		"backend":       func(c *Config) { c.Backend = "disk" },
		"cookie name":   func(c *Config) { c.Name = "bad name" },
		"http only":     func(c *Config) { c.HTTPOnly = false },
		"host prefix":   func(c *Config) { c.Name, c.Domain = "__Host-session", "example.test" },
		"none insecure": func(c *Config) { c.SameSite, c.Secure = "none", false },
		"bad path":      func(c *Config) { c.Path = "relative" },
		"bad domain":    func(c *Config) { c.Domain = "example.test;evil" },
		"idle over ttl": func(c *Config) { c.IdleTTL = c.TTL + time.Second },
		"touch":         func(c *Config) { c.TouchInterval = c.IdleTTL },
		"weak id":       func(c *Config) { c.IDBytes = 16 },
		"redis prefix":  func(c *Config) { c.Backend, c.RedisPrefix = BackendRedis, "bad\nkey" },
		"redis instance": func(c *Config) {
			c.Backend, c.RedisInstance = BackendRedis, "bad instance name"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate(%#v) succeeded", cfg)
			}
		})
	}
}
