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
		"workloads": map[string]any{
			"sast": map[string]any{"queues": map[string]any{"sast": 4}, "concurrency": 2},
			"saas": map[string]any{"queues": map[string]any{"saas": 1}},
		},
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
	if len(cfg.Workloads) != 2 {
		t.Fatalf("workloads = %+v", cfg.Workloads)
	}
	if sast := cfg.Workloads["sast"]; sast.Queues["sast"] != 4 || sast.Concurrency != 2 {
		t.Fatalf("sast workload = %+v", sast)
	}
	if saas := cfg.Workloads["saas"]; saas.Queues["saas"] != 1 || saas.Concurrency != 0 {
		t.Fatalf("saas workload = %+v", saas)
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
		{name: "workload without queues", mutate: func(c *Config) {
			c.Workloads = map[string]WorkloadConfig{"sast": {}}
		}, want: `workload "sast" must declare at least one queue`},
		{name: "workload key whitespace", mutate: func(c *Config) {
			c.Workloads = map[string]WorkloadConfig{" sast": {Queues: map[string]int{"sast": 1}}}
		}, want: "workload key"},
		{name: "workload weight", mutate: func(c *Config) {
			c.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 0}}}
		}, want: "workloads.sast.queues"},
		{name: "workload queue name", mutate: func(c *Config) {
			c.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{" sast": 1}}}
		}, want: "workloads.sast.queues"},
		{name: "workload concurrency", mutate: func(c *Config) {
			c.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}, Concurrency: -1}}
		}, want: `workload "sast" concurrency cannot be negative`},
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
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	clone := cfg.clone()
	clone.Queues["default"] = 9
	clone.Queues["other"] = 1
	workload := clone.Workloads["sast"]
	workload.Queues["sast"] = 9
	workload.Queues["other"] = 1
	clone.Workloads["saas"] = WorkloadConfig{}
	if cfg.Queues["default"] != 1 || len(cfg.Queues) != 1 {
		t.Fatalf("clone mutated source queues: %v", cfg.Queues)
	}
	if len(cfg.Workloads) != 1 || cfg.Workloads["sast"].Queues["sast"] != 1 || len(cfg.Workloads["sast"].Queues) != 1 {
		t.Fatalf("clone mutated source workloads: %v", cfg.Workloads)
	}
}

// TestDefaultQueueMayLiveInAWorkloadsQueues covers the deployment where the
// default queue belongs to a workload: the declared vocabulary is the union of
// every group's set, so naming one of its queues as the default is valid.
func TestDefaultQueueMayLiveInAWorkloadsQueues(t *testing.T) {
	cfg := defaultConfig()
	cfg.Queues = map[string]int{"housekeeping": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	cfg.DefaultQueue = "sast"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	cfg.DefaultQueue = "undeclared"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("Validate() error = %v, want an undeclared default queue refused", err)
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
