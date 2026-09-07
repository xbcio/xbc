package asynq

import (
	"strings"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
)

func TestConfigDefaultsAndStrictBinding(t *testing.T) {
	cfg := bindAsynqConfig(t, map[string]any{})
	if cfg.Redis.Addr != "127.0.0.1:6379" || cfg.Redis.DB != 0 || cfg.Redis.PoolSize != 10 || cfg.Redis.MaxRetries != 3 {
		t.Fatalf("Redis defaults = %+v", cfg.Redis)
	}
	if cfg.Redis.DialTimeout != 5*time.Second || cfg.Redis.ReadTimeout != 3*time.Second || cfg.Redis.WriteTimeout != 3*time.Second {
		t.Fatalf("Redis timeout defaults = %+v", cfg.Redis)
	}
	if cfg.Concurrency != 10 || cfg.StrictPriority || cfg.DefaultQueue != "default" || cfg.DefaultMaxRetries != 25 {
		t.Fatalf("worker defaults = %+v", cfg)
	}
	if cfg.DefaultTimeout != 30*time.Minute || cfg.TaskCheckInterval != time.Second || cfg.ShutdownTimeout != 8*time.Second {
		t.Fatalf("duration defaults = %+v", cfg)
	}
	if len(cfg.Queues) != 1 || cfg.Queues["default"] != 1 {
		t.Fatalf("queue defaults = %v", cfg.Queues)
	}
	if _, err := bindAsynqConfigWithoutValidation(map[string]any{"worker_count": 4}); err == nil {
		t.Fatal("Bind() accepted an unknown field")
	}
}

func TestConfigBindsSupportedOptions(t *testing.T) {
	cfg := bindAsynqConfig(t, map[string]any{
		"redis": map[string]any{
			"addr": "redis.internal:6380", "username": "service", "password": "secret", "db": 4,
			"dial_timeout": "1s", "read_timeout": "2s", "write_timeout": "3s", "pool_size": 20, "max_retries": -1,
		},
		"queues": map[string]any{"critical": 8, "default": 2}, "strict_priority": true, "concurrency": 7,
		"default_queue": "critical", "default_max_retries": 5, "default_timeout": "45s",
		"task_check_interval": "250ms", "shutdown_timeout": "3s",
	})
	if cfg.Redis.Addr != "redis.internal:6380" || cfg.Redis.Username != "service" || cfg.Redis.Password != "secret" || cfg.Redis.DB != 4 {
		t.Fatalf("Redis identity config = %+v", cfg.Redis)
	}
	if cfg.Redis.DialTimeout != time.Second || cfg.Redis.ReadTimeout != 2*time.Second || cfg.Redis.WriteTimeout != 3*time.Second || cfg.Redis.PoolSize != 20 || cfg.Redis.MaxRetries != -1 {
		t.Fatalf("Redis options = %+v", cfg.Redis)
	}
	if !cfg.StrictPriority || cfg.Concurrency != 7 || cfg.Queues["critical"] != 8 || cfg.Queues["default"] != 2 {
		t.Fatalf("worker config = %+v", cfg)
	}
	if cfg.DefaultQueue != "critical" || cfg.DefaultMaxRetries != 5 || cfg.DefaultTimeout != 45*time.Second || cfg.TaskCheckInterval != 250*time.Millisecond || cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("task defaults = %+v", cfg)
	}
	options := cfg.Redis.options()
	if options.Addr != cfg.Redis.Addr || options.DB != cfg.Redis.DB || options.PoolSize != cfg.Redis.PoolSize || options.MaxRetries != cfg.Redis.MaxRetries {
		t.Fatalf("Redis options mapping = %+v", options)
	}
}

func TestConfigSchemaAndCrossFieldValidation(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		field  string
	}{
		{name: "address", values: map[string]any{"redis": map[string]any{"addr": "localhost"}}, field: ".redis.addr"},
		{name: "database", values: map[string]any{"redis": map[string]any{"db": -1}}, field: ".redis.db"},
		{name: "pool", values: map[string]any{"redis": map[string]any{"pool_size": 0}}, field: ".redis.pool_size"},
		{name: "weight", values: map[string]any{"queues": map[string]any{"default": 0}}, field: ".queues"},
		{name: "concurrency", values: map[string]any{"concurrency": 0}, field: ".concurrency"},
		{name: "retries", values: map[string]any{"default_max_retries": -1}, field: ".default_max_retries"},
		{name: "timeout", values: map[string]any{"default_timeout": "0s"}, field: ".default_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := bindAsynqConfigWithoutValidation(test.values)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			err = xbcconfig.Validate(&cfg, "plugins.asynq")
			if err == nil || !strings.Contains(err.Error(), "plugins.asynq"+test.field) {
				t.Fatalf("Validate() error = %v, want field %s", err, test.field)
			}
		})
	}

	base := defaultConfig()
	manual := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "no queues", mutate: func(c *Config) { c.Queues = nil }, want: "at least one queue"},
		{name: "default queue missing", mutate: func(c *Config) { c.DefaultQueue = "critical" }, want: "not present"},
		{name: "queue whitespace", mutate: func(c *Config) { c.Queues = map[string]int{" default": 1}; c.DefaultQueue = " default" }, want: "queue name"},
		{name: "weight overflow", mutate: func(c *Config) { max := int(^uint(0) >> 1); c.Queues = map[string]int{"default": max, "other": 1} }, want: "overflow"},
		{name: "bad Redis port", mutate: func(c *Config) { c.Redis.Addr = "localhost:70000" }, want: "invalid port"},
		{name: "bad Redis retries", mutate: func(c *Config) { c.Redis.MaxRetries = -2 }, want: "at least -1"},
		{name: "shutdown timeout", mutate: func(c *Config) { c.ShutdownTimeout = 0 }, want: "shutdown_timeout"},
	}
	for _, test := range manual {
		t.Run(test.name, func(t *testing.T) {
			cfg := base.clone()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigCloneDefensivelyCopiesQueues(t *testing.T) {
	cfg := defaultConfig()
	clone := cfg.clone()
	clone.Queues["default"] = 9
	clone.Queues["other"] = 1
	if cfg.Queues["default"] != 1 || len(cfg.Queues) != 1 {
		t.Fatalf("clone mutated source queues: %v", cfg.Queues)
	}
}

func bindAsynqConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	cfg, err := bindAsynqConfigWithoutValidation(values)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := xbcconfig.Validate(&cfg, "plugins.asynq"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate() error = %v", err)
	}
	return cfg
}

func bindAsynqConfigWithoutValidation(values map[string]any) (Config, error) {
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{"asynq": values},
	}, "XBC_ASYNQ_TEST_")
	if err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	if err := environment.Bind("plugins.asynq", &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
