package asynq

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config configures the enqueue client and worker bound at plugins.asynq.
type Config struct {
	Redis RedisConfig `yaml:"redis"`

	// Queues maps queue names to positive relative weights. For example,
	// {critical: 6, default: 3, low: 1} gives those queues approximately
	// 60%, 30%, and 10% of polling opportunities while all remain busy.
	Queues         map[string]int `yaml:"queues" validate:"required,min=1,dive,gt=0"`
	StrictPriority bool           `yaml:"strict_priority" default:"false"`
	Concurrency    int            `yaml:"concurrency"     default:"10" validate:"min=1"`

	DefaultQueue      string        `yaml:"default_queue"       default:"default" validate:"required"`
	DefaultMaxRetries int           `yaml:"default_max_retries" default:"25"      validate:"min=0"`
	DefaultTimeout    time.Duration `yaml:"default_timeout"     default:"30m"     validate:"gt=0"`

	TaskCheckInterval time.Duration `yaml:"task_check_interval" default:"1s" validate:"gt=0"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"   default:"8s" validate:"gt=0"`
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
	out.Queues = make(map[string]int, len(c.Queues))
	for name, weight := range c.Queues {
		out.Queues[name] = weight
	}
	return out
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
	maxInt := int(^uint(0) >> 1)
	totalWeight := 0
	for name, weight := range c.Queues {
		if strings.TrimSpace(name) != name || name == "" {
			return fmt.Errorf("asynq: queue name %q must be non-empty and have no surrounding whitespace", name)
		}
		if weight <= 0 {
			return fmt.Errorf("asynq: queue %q weight must be positive, got %d", name, weight)
		}
		if weight > maxInt-totalWeight {
			return fmt.Errorf("asynq: queue weights overflow int")
		}
		totalWeight += weight
	}
	if strings.TrimSpace(c.DefaultQueue) != c.DefaultQueue || c.DefaultQueue == "" {
		return fmt.Errorf("asynq: default_queue must be non-empty and have no surrounding whitespace")
	}
	if _, ok := c.Queues[c.DefaultQueue]; !ok {
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
