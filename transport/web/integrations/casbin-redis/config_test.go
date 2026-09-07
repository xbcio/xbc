package casbinredis

import (
	"strings"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
)

func TestDefaultConfig(t *testing.T) {
	first := DefaultConfig()
	second := DefaultConfig()
	if first.Mode != ModeStandalone || len(first.Addrs) != 1 || first.Addrs[0] != defaultAddress {
		t.Fatalf("DefaultConfig topology = %#v", first)
	}
	if first.DialTimeout != defaultDialTimeout || first.ReadTimeout != defaultReadTimeout || first.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("DefaultConfig timeouts = %#v", first)
	}
	if first.Channel != defaultChannel || !first.IgnoreSelf || first.DB != 0 {
		t.Fatalf("DefaultConfig watcher settings = %#v", first)
	}
	first.Addrs[0] = "changed:6379"
	if second.Addrs[0] != defaultAddress {
		t.Fatal("DefaultConfig returned aliased address slices")
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}
}

func TestConfigBindingAppliesDefaultsAndHonorsExplicitFalse(t *testing.T) {
	cfg, err := bindConfig(map[string]any{"ignore_self": false})
	if err != nil {
		t.Fatalf("bindConfig() error = %v", err)
	}
	if err := xbcconfig.Validate(&cfg, "plugins.casbin-redis.authorization"); err != nil {
		t.Fatalf("config schema validation error = %v", err)
	}
	if cfg.Mode != ModeStandalone || len(cfg.Addrs) != 1 || cfg.Addrs[0] != defaultAddress {
		t.Fatalf("bound topology defaults = %#v", cfg)
	}
	if cfg.DialTimeout != 5*time.Second || cfg.ReadTimeout != 3*time.Second || cfg.WriteTimeout != 3*time.Second {
		t.Fatalf("bound timeout defaults = %#v", cfg)
	}
	if cfg.Channel != defaultChannel {
		t.Fatalf("Channel = %q, want %q", cfg.Channel, defaultChannel)
	}
	if cfg.IgnoreSelf {
		t.Fatal("explicit ignore_self:false was overwritten by the default")
	}
}

func TestConfigBindingRejectsUnknownFields(t *testing.T) {
	_, err := bindConfig(map[string]any{"addr": "127.0.0.1:6379"})
	if err == nil || !strings.Contains(err.Error(), "addr") {
		t.Fatalf("bindConfig() error = %v, want unknown addr field", err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "unknown mode", mutate: func(c *Config) { c.Mode = "sentinel" }, wantErr: "mode"},
		{name: "missing addresses", mutate: func(c *Config) { c.Addrs = nil }, wantErr: "addrs"},
		{name: "multiple standalone addresses", mutate: func(c *Config) { c.Addrs = []string{"one:6379", "two:6379"} }, wantErr: "exactly one"},
		{name: "missing port", mutate: func(c *Config) { c.Addrs = []string{"localhost"} }, wantErr: "host:port"},
		{name: "empty host", mutate: func(c *Config) { c.Addrs = []string{":6379"} }, wantErr: "empty host"},
		{name: "invalid port", mutate: func(c *Config) { c.Addrs = []string{"localhost:70000"} }, wantErr: "invalid port"},
		{name: "invalid host", mutate: func(c *Config) { c.Addrs = []string{"bad_host:6379"} }, wantErr: "invalid host"},
		{name: "duplicate cluster seed", mutate: func(c *Config) { c.Mode = ModeCluster; c.Addrs = []string{"redis-a:6379", "REDIS-A:6379"} }, wantErr: "duplicates"},
		{name: "negative database", mutate: func(c *Config) { c.DB = -1 }, wantErr: "db"},
		{name: "cluster database", mutate: func(c *Config) { c.Mode = ModeCluster; c.DB = 1 }, wantErr: "cluster mode"},
		{name: "dial timeout", mutate: func(c *Config) { c.DialTimeout = 0 }, wantErr: "dial_timeout"},
		{name: "read timeout", mutate: func(c *Config) { c.ReadTimeout = -time.Second }, wantErr: "read_timeout"},
		{name: "write timeout", mutate: func(c *Config) { c.WriteTimeout = 0 }, wantErr: "write_timeout"},
		{name: "empty channel", mutate: func(c *Config) { c.Channel = "  " }, wantErr: "channel"},
		{name: "channel control character", mutate: func(c *Config) { c.Channel = "/casbin\nother" }, wantErr: "channel"},
		{name: "username control character", mutate: func(c *Config) { c.Username = "service\raccount" }, wantErr: "username"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Password = "never-print-this-password"
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), cfg.Password) {
				t.Fatalf("Validate() leaked password: %v", err)
			}
		})
	}
}

func TestConfigAcceptsStandaloneAndClusterTopologies(t *testing.T) {
	standalone := DefaultConfig()
	standalone.Addrs = []string{"[::1]:6379"}
	standalone.Username = "casbin"
	standalone.Password = "secret"
	standalone.DB = 3
	if err := standalone.Validate(); err != nil {
		t.Fatalf("standalone.Validate() error = %v", err)
	}

	cluster := DefaultConfig()
	cluster.Mode = ModeCluster
	cluster.Addrs = []string{"redis-a.internal:6379", "10.0.0.2:6380", "[2001:db8::1]:6381"}
	if err := cluster.Validate(); err != nil {
		t.Fatalf("cluster.Validate() error = %v", err)
	}
}

func TestSchemaValidationMasksPassword(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Password = "schema-secret-value"
	cfg.DialTimeout = 0
	err := xbcconfig.Validate(&cfg, "plugins.casbin-redis.authorization")
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatalf("schema validation leaked password: %v", err)
	}
}

func bindConfig(values map[string]any) (Config, error) {
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"casbin-redis": map[string]any{
				"authorization": values,
			},
		},
	}, "XBC_CASBIN_REDIS_TEST_")
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := environment.Bind("plugins.casbin-redis.authorization", &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
