package redissyncmap

import (
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/plugin"
)

// Config configures one redis-syncmap factory. XBC binds it from one named
// section below plugins.redis-syncmap, for example
// plugins.redis-syncmap.default.
type Config struct {
	// RedisInstance names the configured entry below plugins.redis that backs
	// every map this factory creates.
	RedisInstance string `yaml:"redis_instance" default:"default"`
	// Namespace is the key prefix shared by the maps of every replica that
	// must see each other. Two deployments that use the same Redis instance
	// and the same namespace share map state; give each logical cluster its own
	// namespace.
	Namespace string `yaml:"namespace" default:"xbc" validate:"required"`
	// Persist selects what changes are kept in Redis: "none" broadcasts only,
	// "ttl" stores the map and applies TTL to the stored collection.
	Persist string `yaml:"persist" default:"none" validate:"required,oneof=none ttl"`
	// TTL is the expiry applied to the whole stored collection in "ttl" mode.
	// It is required and positive in that mode, and ignored in "none" mode.
	TTL time.Duration `yaml:"ttl" default:"0s" validate:"min=0"`
	// MaxSize bounds each map's local cache with LRU eviction. Zero means
	// unlimited. Eviction is local only: it never deletes from Redis and never
	// broadcasts.
	MaxSize int `yaml:"max_size" default:"0" validate:"min=0"`
}

// DefaultConfig returns the configuration a section that names nothing gets.
func DefaultConfig() Config {
	return Config{
		RedisInstance: plugin.DefaultInstance,
		Namespace:     "xbc",
		Persist:       string(syncmap.PersistNone),
		TTL:           0,
		MaxSize:       0,
	}
}

// prepareConfig enforces every configuration-only invariant. It performs no
// I/O and opens no connection, so it runs during planning.
func prepareConfig(config Config) (Config, error) {
	config.RedisInstance = strings.TrimSpace(config.RedisInstance)
	if config.RedisInstance == "" {
		config.RedisInstance = plugin.DefaultInstance
	}
	if err := plugin.ValidateInstanceName(config.RedisInstance); err != nil {
		return Config{}, fmt.Errorf("redis-syncmap: invalid redis_instance: %w", err)
	}

	config.Namespace = strings.TrimSpace(config.Namespace)
	if err := plugin.ValidateInstanceName(config.Namespace); err != nil {
		return Config{}, fmt.Errorf("redis-syncmap: invalid namespace: %w", err)
	}

	config.Persist = strings.ToLower(strings.TrimSpace(config.Persist))
	switch syncmap.PersistMode(config.Persist) {
	case syncmap.PersistNone:
	case syncmap.PersistTTL:
		if config.TTL <= 0 {
			return Config{}, fmt.Errorf("redis-syncmap: persist ttl requires a positive ttl, got %s", config.TTL)
		}
	default:
		return Config{}, fmt.Errorf("redis-syncmap: unknown persist mode %q, want none or ttl", config.Persist)
	}

	if config.MaxSize < 0 {
		return Config{}, fmt.Errorf("redis-syncmap: max_size cannot be negative, got %d", config.MaxSize)
	}
	return config, nil
}

func (c Config) mode() syncmap.PersistMode { return syncmap.PersistMode(c.Persist) }
