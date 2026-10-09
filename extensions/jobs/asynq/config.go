package asynq

import (
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config configures the enqueue client and worker bound at plugins.asynq.
//
// The worker is one server per group of contributing Plugins: the Plugins that
// belong to a workload are served by that workload's server, and the Plugins
// that belong to none share the top-level one. One group consumes one queue
// set, so handlers that belong together are consumed by the process that
// carries them and a workload this process does not host gets no worker.
type Config struct {
	Redis RedisConfig `yaml:"redis"`

	// Queues maps queue names to positive relative weights. For example,
	// {critical: 6, default: 3, low: 1} gives those queues approximately
	// 60%, 30%, and 10% of polling opportunities while all remain busy. These
	// are the unowned group's queues; a workload declares its own under
	// workloads.
	Queues         map[string]int `yaml:"queues" validate:"required,min=1,dive,gt=0"`
	StrictPriority bool           `yaml:"strict_priority" default:"false"`
	Concurrency    int            `yaml:"concurrency"     default:"10" validate:"min=1"`

	// Workloads declares the worker each workload gets when its Plugins
	// contribute task handlers. A workload with contributors must name its
	// queues here: which queue a workload's tasks are consumed from is a
	// deployment decision, not something derivable from a handler's task type.
	// An entry for a workload that contributes nothing here is unused, which is
	// the ordinary case of a workload this process does not host.
	Workloads map[string]WorkloadConfig `yaml:"workloads"`

	DefaultQueue      string        `yaml:"default_queue"       default:"default" validate:"required"`
	DefaultMaxRetries int           `yaml:"default_max_retries" default:"25"      validate:"min=0"`
	DefaultTimeout    time.Duration `yaml:"default_timeout"     default:"30m"     validate:"gt=0"`

	TaskCheckInterval time.Duration `yaml:"task_check_interval" default:"1s" validate:"gt=0"`

	// ShutdownTimeout bounds the asynq library's worker Shutdown, which Stop
	// runs after Drain has already waited for the handlers still running. A
	// handler that also outlives this budget is requeued by the asynq library
	// and must not be relied on to finish.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"8s" validate:"gt=0"`
}

// WorkloadConfig is one workload's worker: the queues it consumes and how many
// tasks it runs at once.
type WorkloadConfig struct {
	// Queues is the workload's queue set, with the same weight semantics as the
	// top-level queues field. It is required for a workload whose Plugins
	// contribute handlers, and must not overlap another group's set: a queue
	// consumed by two servers in one process is delivered to whichever server
	// fetches it first, which is a routing bug rather than a deployment choice.
	Queues map[string]int `yaml:"queues"`
	// Concurrency bounds how many tasks this workload's worker runs at once.
	// Zero takes the top-level concurrency.
	Concurrency int `yaml:"concurrency"`
}

// RedisConfig configures the Redis connection owned exclusively by the plugin.
type RedisConfig struct {
	Addr     string `yaml:"addr"     default:"127.0.0.1:6379" validate:"required,hostname_port"`
	Username string `yaml:"username"`
	Password string `yaml:"password" mask:"true"`
	DB       int    `yaml:"db"       default:"0" validate:"min=0"`

	DialTimeout  time.Duration `yaml:"dial_timeout"  default:"5s" validate:"gt=0"`
	ReadTimeout  time.Duration `yaml:"read_timeout"  default:"3s" validate:"gt=0"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"3s" validate:"gt=0"`
	PoolSize     int           `yaml:"pool_size"     default:"10" validate:"min=1"`
	MaxRetries   int           `yaml:"max_retries"   default:"3"  validate:"min=-1"`
}

func defaultConfig() Config {
	return Config{
		Redis: RedisConfig{
			Addr:         "127.0.0.1:6379",
			DialTimeout:  5 * time.Second,
			ReadTimeout:  3 * time.Second,
			WriteTimeout: 3 * time.Second,
			PoolSize:     10,
			MaxRetries:   3,
		},
		Queues:            map[string]int{"default": 1},
		Concurrency:       10,
		DefaultQueue:      "default",
		DefaultMaxRetries: 25,
		DefaultTimeout:    30 * time.Minute,
		TaskCheckInterval: time.Second,
		ShutdownTimeout:   8 * time.Second,
	}
}

func (c Config) clone() Config {
	out := c
	out.Queues = maps.Clone(c.Queues)
	if c.Workloads != nil {
		out.Workloads = make(map[string]WorkloadConfig, len(c.Workloads))
		for key, workload := range c.Workloads {
			workload.Queues = maps.Clone(workload.Queues)
			out.Workloads[key] = workload
		}
	}
	return out
}

// knownQueues is every queue name this configuration declares: the unowned
// group's set plus each workload's.
//
// Enqueue validation is against this whole vocabulary rather than the queues
// this process consumes. A process enqueues legitimately to workloads it does
// not host -- an API process that accepts sast work and hands it to the sast
// worker over the same queue -- so restricting the client to the local groups
// would break composition in exactly the deployment the split exists for.
func (c Config) knownQueues() map[string]struct{} {
	declared := make(map[string]struct{}, len(c.Queues))
	for name := range c.Queues {
		declared[name] = struct{}{}
	}
	for _, workload := range c.Workloads {
		for name := range workload.Queues {
			declared[name] = struct{}{}
		}
	}
	return declared
}

// Validate checks all connection, queue, retry, timeout, and cross-field
// constraints that are not expressible through struct tags alone.
func (c Config) Validate() error { return c.validate() }

func (c Config) validate() error {
	if err := c.Redis.validate(); err != nil {
		return err
	}
	if c.Concurrency <= 0 {
		return fmt.Errorf("asynq: concurrency must be positive, got %d", c.Concurrency)
	}
	if len(c.Queues) == 0 {
		return fmt.Errorf("asynq: at least one queue is required")
	}
	if err := validateQueues(c.Queues, "queues"); err != nil {
		return err
	}
	for key, workload := range c.Workloads {
		if strings.TrimSpace(key) != key || key == "" {
			return fmt.Errorf("asynq: workload key %q must be non-empty and have no surrounding whitespace", key)
		}
		if len(workload.Queues) == 0 {
			return fmt.Errorf("asynq: workload %q must declare at least one queue", key)
		}
		if err := validateQueues(workload.Queues, fmt.Sprintf("workloads.%s.queues", key)); err != nil {
			return err
		}
		if workload.Concurrency < 0 {
			return fmt.Errorf("asynq: workload %q concurrency cannot be negative, got %d", key, workload.Concurrency)
		}
	}
	if strings.TrimSpace(c.DefaultQueue) != c.DefaultQueue || c.DefaultQueue == "" {
		return fmt.Errorf("asynq: default_queue must be non-empty and have no surrounding whitespace")
	}
	if _, ok := c.knownQueues()[c.DefaultQueue]; !ok {
		return fmt.Errorf("asynq: default_queue %q is not present in queues", c.DefaultQueue)
	}
	if c.DefaultMaxRetries < 0 {
		return fmt.Errorf("asynq: default_max_retries cannot be negative, got %d", c.DefaultMaxRetries)
	}
	if c.DefaultTimeout <= 0 {
		return fmt.Errorf("asynq: default_timeout must be positive, got %s", c.DefaultTimeout)
	}
	if c.TaskCheckInterval <= 0 {
		return fmt.Errorf("asynq: task_check_interval must be positive, got %s", c.TaskCheckInterval)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("asynq: shutdown_timeout must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}

// validateQueues checks one queue set's names and weights. where is the
// configuration path the set came from, so an error in a workload's set does
// not read as if it were the top-level one.
func validateQueues(queues map[string]int, where string) error {
	maxInt := int(^uint(0) >> 1)
	totalWeight := 0
	for name, weight := range queues {
		if strings.TrimSpace(name) != name || name == "" {
			return fmt.Errorf("asynq: %s queue name %q must be non-empty and have no surrounding whitespace", where, name)
		}
		if weight <= 0 {
			return fmt.Errorf("asynq: %s queue %q weight must be positive, got %d", where, name, weight)
		}
		if weight > maxInt-totalWeight {
			return fmt.Errorf("asynq: %s queue weights overflow int", where)
		}
		totalWeight += weight
	}
	return nil
}

func (c RedisConfig) validate() error {
	if strings.TrimSpace(c.Addr) != c.Addr || c.Addr == "" {
		return fmt.Errorf("asynq: redis.addr must be non-empty and have no surrounding whitespace")
	}
	host, portText, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" {
		return fmt.Errorf("asynq: redis.addr %q must be a host:port address", c.Addr)
	}
	port, portErr := strconv.Atoi(portText)
	if portErr != nil || port < 1 || port > 65535 {
		return fmt.Errorf("asynq: redis.addr %q has an invalid port", c.Addr)
	}
	if c.DB < 0 {
		return fmt.Errorf("asynq: redis.db cannot be negative, got %d", c.DB)
	}
	if c.DialTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 {
		return fmt.Errorf("asynq: redis dial/read/write timeouts must be positive")
	}
	if c.PoolSize <= 0 {
		return fmt.Errorf("asynq: redis.pool_size must be positive, got %d", c.PoolSize)
	}
	if c.MaxRetries < -1 {
		return fmt.Errorf("asynq: redis.max_retries must be at least -1, got %d", c.MaxRetries)
	}
	return nil
}

func (c RedisConfig) options() *goredis.Options {
	return &goredis.Options{
		Addr:         c.Addr,
		Username:     c.Username,
		Password:     c.Password,
		DB:           c.DB,
		DialTimeout:  c.DialTimeout,
		ReadTimeout:  c.ReadTimeout,
		WriteTimeout: c.WriteTimeout,
		PoolSize:     c.PoolSize,
		MaxRetries:   c.MaxRetries,
	}
}
