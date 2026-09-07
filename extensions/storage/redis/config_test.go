package redis

import (
	"strings"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
)

func TestConfigDefaults(t *testing.T) {
	cfg := bindConfig(t, map[string]any{})

	if cfg.Addr != "127.0.0.1:6379" {
		t.Fatalf("Addr = %q, want default address", cfg.Addr)
	}
	if cfg.DB != 0 || cfg.Username != "" || cfg.Password != "" {
		t.Fatalf("unexpected identity defaults: %+v", cfg)
	}
	if cfg.DialTimeout != 5*time.Second || cfg.ReadTimeout != 3*time.Second || cfg.WriteTimeout != 3*time.Second || cfg.PoolTimeout != 4*time.Second {
		t.Fatalf("unexpected timeout defaults: %+v", cfg)
	}
	if cfg.PoolSize != 10 || cfg.MinIdleConns != 0 || cfg.MaxIdleConns != 0 || cfg.MaxActiveConns != 0 {
		t.Fatalf("unexpected pool defaults: %+v", cfg)
	}
	if cfg.ConnMaxIdleTime != 30*time.Minute || cfg.ConnMaxLifetime != 0 || cfg.MaxRetries != 3 {
		t.Fatalf("unexpected connection defaults: %+v", cfg)
	}
	if !cfg.Ping {
		t.Fatal("Ping = false, want true by default")
	}
}

func TestConfigBindsAllSupportedOptions(t *testing.T) {
	cfg := bindConfig(t, map[string]any{
		"addr":               "redis.internal:6380",
		"username":           "service",
		"password":           "secret",
		"db":                 7,
		"dial_timeout":       "1s",
		"read_timeout":       "2s",
		"write_timeout":      "3s",
		"pool_timeout":       "4s",
		"pool_size":          20,
		"min_idle_conns":     2,
		"max_idle_conns":     8,
		"max_active_conns":   30,
		"conn_max_idle_time": "5m",
		"conn_max_lifetime":  "1h",
		"max_retries":        5,
		"ping":               false,
	})

	options := cfg.options()
	if options.Addr != cfg.Addr || options.Username != cfg.Username || options.Password != cfg.Password || options.DB != cfg.DB {
		t.Fatalf("identity options do not match config: %#v", options)
	}
	if options.DialTimeout != cfg.DialTimeout || options.ReadTimeout != cfg.ReadTimeout || options.WriteTimeout != cfg.WriteTimeout || options.PoolTimeout != cfg.PoolTimeout {
		t.Fatalf("timeout options do not match config: %#v", options)
	}
	if options.PoolSize != cfg.PoolSize || options.MinIdleConns != cfg.MinIdleConns || options.MaxIdleConns != cfg.MaxIdleConns || options.MaxActiveConns != cfg.MaxActiveConns {
		t.Fatalf("pool options do not match config: %#v", options)
	}
	if options.ConnMaxIdleTime != cfg.ConnMaxIdleTime || options.ConnMaxLifetime != cfg.ConnMaxLifetime || options.MaxRetries != cfg.MaxRetries {
		t.Fatalf("connection options do not match config: %#v", options)
	}
	if cfg.Ping {
		t.Fatal("explicit ping=false was overwritten by its default")
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		field  string
	}{
		{name: "address", values: map[string]any{"addr": "localhost"}, field: ".addr"},
		{name: "database", values: map[string]any{"db": -1}, field: ".db"},
		{name: "dial timeout", values: map[string]any{"dial_timeout": "0s"}, field: ".dial_timeout"},
		{name: "pool size", values: map[string]any{"pool_size": 0}, field: ".pool_size"},
		{name: "minimum idle exceeds pool", values: map[string]any{"pool_size": 2, "min_idle_conns": 3}, field: ".min_idle_conns"},
		{name: "maximum idle below minimum", values: map[string]any{"pool_size": 10, "min_idle_conns": 4, "max_idle_conns": 3}, field: ".max_idle_conns"},
		{name: "active limit below pool", values: map[string]any{"pool_size": 10, "max_active_conns": 5}, field: ".max_active_conns"},
		{name: "negative lifetime", values: map[string]any{"conn_max_lifetime": "-1s"}, field: ".conn_max_lifetime"},
		{name: "invalid retries", values: map[string]any{"max_retries": -2}, field: ".max_retries"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := bindConfigWithoutValidation(test.values)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			err = xbcconfig.Validate(&cfg, "plugins.redis.default")
			if err == nil {
				t.Fatal("Validate() error = nil, want validation failure")
			}
			if !strings.Contains(err.Error(), "plugins.redis.default"+test.field) {
				t.Fatalf("Validate() error = %q, want field %q", err, test.field)
			}
		})
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	_, err := bindConfigWithoutValidation(map[string]any{"address": "127.0.0.1:6379"})
	if err == nil {
		t.Fatal("Bind() error = nil, want strict unknown-field failure")
	}
}

func bindConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	cfg, err := bindConfigWithoutValidation(values)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := xbcconfig.Validate(&cfg, "plugins.redis.default"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	return cfg
}

func bindConfigWithoutValidation(values map[string]any) (Config, error) {
	env, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis": map[string]any{
				"default": values,
			},
		},
	}, "XBC_REDIS_TEST_")
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := env.Bind("plugins.redis.default", &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
