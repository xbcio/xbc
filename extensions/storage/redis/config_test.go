package redis

import (
	"fmt"
	"strings"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestConfigDefaults(t *testing.T) {
	cfg := bindConfig(t, map[string]any{})

	if cfg.Mode != ModeStandalone {
		t.Fatalf("Mode = %q, want standalone by default", cfg.Mode)
	}
	if cfg.Addr != "127.0.0.1:6379" {
		t.Fatalf("Addr = %q, want default address", cfg.Addr)
	}
	if cfg.DB != 0 || cfg.Username != "" || cfg.Password != "" {
		t.Fatalf("unexpected identity defaults: %+v", cfg)
	}
	if len(cfg.Addrs) != 0 || cfg.MasterName != "" || cfg.SentinelUsername != "" || cfg.SentinelPassword != "" {
		t.Fatalf("unexpected topology defaults: %+v", cfg)
	}
	if cfg.RouteByLatency || cfg.RouteRandomly || cfg.ReadOnly || cfg.MaxRedirects != 0 {
		t.Fatalf("unexpected topology flag defaults: %+v", cfg)
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

func TestConfigBindsTopologyFields(t *testing.T) {
	sentinel := bindConfig(t, map[string]any{
		"mode":              "sentinel",
		"addrs":             []any{"10.0.0.1:26379", "10.0.0.2:26379"},
		"master_name":       "primary",
		"sentinel_username": "watcher",
		"sentinel_password": "sentinel-secret",
		"route_by_latency":  true,
		"db":                3,
	})
	if sentinel.Mode != ModeSentinel {
		t.Fatalf("Mode = %q, want sentinel", sentinel.Mode)
	}
	if len(sentinel.Addrs) != 2 || sentinel.Addrs[0] != "10.0.0.1:26379" || sentinel.Addrs[1] != "10.0.0.2:26379" {
		t.Fatalf("Addrs = %#v, want both sentinel addresses", sentinel.Addrs)
	}
	if sentinel.MasterName != "primary" || sentinel.SentinelUsername != "watcher" || sentinel.SentinelPassword != "sentinel-secret" {
		t.Fatalf("sentinel identity = %+v", sentinel)
	}
	if !sentinel.RouteByLatency || sentinel.RouteRandomly {
		t.Fatalf("sentinel route flags = %+v", sentinel)
	}
	if sentinel.DB != 3 {
		t.Fatalf("DB = %d, want the configured database to remain available in sentinel mode", sentinel.DB)
	}

	cluster := bindConfig(t, map[string]any{
		"mode":          "cluster",
		"addrs":         []any{"10.0.0.1:6379", "10.0.0.2:6379"},
		"read_only":     true,
		"max_redirects": 5,
	})
	if cluster.Mode != ModeCluster || !cluster.ReadOnly || cluster.MaxRedirects != 5 {
		t.Fatalf("cluster topology = %+v", cluster)
	}
	if cluster.Addr != "127.0.0.1:6379" {
		t.Fatalf("Addr = %q; the standalone default stays in place and is documented as ignored", cluster.Addr)
	}
}

// TestPrepareConfigEnforcesModeFields drives every case through the
// Definition's own ConfigSpec.Prepare, in the order assembly runs it: defaults,
// bind, validate tags, prepare. Driving prepareConfig directly would leave the
// wiring untested, and a ConfigSpec without Prepare accepts every
// configuration the reject cases below describe.
func TestPrepareConfigEnforcesModeFields(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		reject string   // substring every rejection must carry
		fields []string // configuration spellings the message must name
	}{
		{name: "standalone by default", values: map[string]any{}},
		{name: "standalone spelled out", values: map[string]any{"mode": "standalone", "addr": "redis.internal:6380", "db": 2}},
		{name: "sentinel", values: map[string]any{
			"mode": "sentinel", "addrs": []any{"10.0.0.1:26379", "10.0.0.2:26379"}, "master_name": "primary",
			"sentinel_username": "watcher", "sentinel_password": "sentinel-secret", "route_randomly": true, "db": 1,
		}},
		{name: "cluster", values: map[string]any{
			"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "read_only": true, "max_redirects": 5,
		}},
		{name: "cluster disables redirects", values: map[string]any{
			"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "max_redirects": -1,
		}},

		{name: "standalone rejects a seed list", values: map[string]any{"addrs": []any{"10.0.0.1:26379"}},
			reject: "mode standalone does not accept", fields: []string{"addrs"}},
		{name: "standalone rejects every other mode's fields", values: map[string]any{
			"addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "sentinel_password": "sentinel-secret",
			"route_by_latency": true, "read_only": true, "max_redirects": 5,
		}, reject: "mode standalone does not accept", fields: []string{
			"addrs", "master_name", "sentinel_password", "route_by_latency", "read_only", "max_redirects",
		}},
		{name: "sentinel requires master_name", values: map[string]any{"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}},
			reject: "requires master_name"},
		{name: "sentinel requires addrs", values: map[string]any{"mode": "sentinel", "master_name": "primary"},
			reject: "requires addrs"},
		{name: "sentinel rejects cluster fields", values: map[string]any{
			"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "read_only": true, "max_redirects": 2,
		}, reject: "mode sentinel does not accept", fields: []string{"read_only", "max_redirects"}},
		{name: "cluster requires addrs", values: map[string]any{"mode": "cluster"},
			reject: "requires addrs"},
		{name: "cluster rejects sentinel fields", values: map[string]any{
			"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "master_name": "primary",
			"sentinel_username": "watcher", "route_randomly": true,
		}, reject: "mode cluster does not accept", fields: []string{"master_name", "sentinel_username", "route_randomly"}},
		{name: "cluster rejects a database", values: map[string]any{"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "db": 1},
			reject: "mode cluster does not support db", fields: []string{"db"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := bindPreparedConfig(test.values)
			if test.reject == "" {
				if err != nil {
					t.Fatalf("Prepare() error = %v, want acceptance", err)
				}
				if cfg.Mode == "" {
					t.Fatal("Prepare() returned a configuration without a mode")
				}
				return
			}
			if err == nil {
				t.Fatal("Prepare() error = nil, want rejection")
			}
			if !strings.Contains(err.Error(), test.reject) {
				t.Fatalf("Prepare() error = %q, want %q", err, test.reject)
			}
			for _, field := range test.fields {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("Prepare() error = %q, want it to name %q", err, field)
				}
			}
		})
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
		{name: "unknown mode", values: map[string]any{"mode": "replica"}, field: ".mode"},
		{name: "seed address", values: map[string]any{"mode": "sentinel", "master_name": "primary", "addrs": []any{"10.0.0.1:26379", "localhost"}}, field: ".addrs[1]"},
		{name: "redirects below the library floor", values: map[string]any{"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "max_redirects": -2}, field: ".max_redirects"},
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

// preparedConfig returns a configuration that has passed the whole assembly
// path, so a construction test can rely on running only on sections the plugin
// itself admits.
func preparedConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	cfg, err := bindPreparedConfig(values)
	if err != nil {
		t.Fatalf("prepared config: %v", err)
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

// bindPreparedConfig runs the assembly path for one plugins.redis.default
// section: bind with defaults, validate the tags, then prepare. Prepare is
// taken from the Definition rather than called directly so that a ConfigSpec
// which stopped wiring it fails here.
func bindPreparedConfig(values map[string]any) (Config, error) {
	cfg, err := bindConfigWithoutValidation(values)
	if err != nil {
		return Config{}, err
	}
	if err := xbcconfig.Validate(&cfg, "plugins.redis.default"); err != nil {
		return Config{}, err
	}
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
	if !ok || descriptor.Config == nil || descriptor.Config.Prepare == nil {
		return Config{}, fmt.Errorf("redis definition declares no configuration Prepare")
	}
	prepared, err := descriptor.Config.Prepare(cfg)
	if err != nil {
		return Config{}, err
	}
	result, ok := prepared.(Config)
	if !ok {
		return Config{}, fmt.Errorf("Prepare returned %T, want Config", prepared)
	}
	return result, nil
}
