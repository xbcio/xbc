package idempotency

import (
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/plugin"
)

const (
	BackendMemory = "memory"
	BackendRedis  = "redis"
)

// Config is bound from plugins.idempotency.
type Config struct {
	Header           string        `yaml:"header"             default:"Idempotency-Key"`
	Backend          string        `yaml:"backend"            default:"memory"`
	RedisInstance    string        `yaml:"redis_instance"     default:"default"`
	RedisPrefix      string        `yaml:"redis_prefix"       default:"xbc:idempotency:"`
	TTL              time.Duration `yaml:"ttl"                default:"24h" validate:"gt=0"`
	PendingTTL       time.Duration `yaml:"pending_ttl"        default:"30s" validate:"gt=0"`
	OperationTimeout time.Duration `yaml:"operation_timeout"  default:"2s" validate:"gt=0"`
	PendingStatus    int           `yaml:"pending_status"     default:"425" validate:"oneof=409 425"`
	MinKeyLength     int           `yaml:"min_key_length"     default:"8" validate:"min=1,max=128"`
	MaxKeyLength     int           `yaml:"max_key_length"     default:"128" validate:"min=8,max=512"`
	MaxRequestBytes  int64         `yaml:"max_request_bytes"  default:"1048576" validate:"gt=0"`
	MaxResponseBytes int64         `yaml:"max_response_bytes" default:"1048576" validate:"gt=0"`
}

type normalizedConfig struct {
	header           string
	backend          string
	redisInstance    string
	redisPrefix      string
	ttl              time.Duration
	pendingTTL       time.Duration
	operationTimeout time.Duration
	pendingStatus    int
	minKeyLength     int
	maxKeyLength     int
	maxRequestBytes  int64
	maxResponseBytes int64
}

// DefaultConfig returns the bounded defaults applied when a field is unset.
func DefaultConfig() Config {
	return Config{
		Header:           "Idempotency-Key",
		Backend:          BackendMemory,
		RedisInstance:    "default",
		RedisPrefix:      "xbc:idempotency:",
		TTL:              24 * time.Hour,
		PendingTTL:       30 * time.Second,
		OperationTimeout: 2 * time.Second,
		PendingStatus:    425,
		MinKeyLength:     8,
		MaxKeyLength:     128,
		MaxRequestBytes:  1 << 20,
		MaxResponseBytes: 1 << 20,
	}
}

// Validate checks semantic constraints in addition to struct tags.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// acquiring any resource and returns it unchanged so plan(cfg) can rely on
// already-defaulted, already-validated values.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := DefaultConfig()
	if strings.TrimSpace(c.Header) == "" {
		c.Header = d.Header
	}
	if strings.TrimSpace(c.Backend) == "" {
		c.Backend = d.Backend
	}
	if strings.TrimSpace(c.RedisInstance) == "" {
		c.RedisInstance = d.RedisInstance
	}
	if c.RedisPrefix == "" {
		c.RedisPrefix = d.RedisPrefix
	}
	if c.TTL == 0 {
		c.TTL = d.TTL
	}
	if c.PendingTTL == 0 {
		c.PendingTTL = d.PendingTTL
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = d.OperationTimeout
	}
	if c.PendingStatus == 0 {
		c.PendingStatus = d.PendingStatus
	}
	if c.MinKeyLength == 0 {
		c.MinKeyLength = d.MinKeyLength
	}
	if c.MaxKeyLength == 0 {
		c.MaxKeyLength = d.MaxKeyLength
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = d.MaxRequestBytes
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = d.MaxResponseBytes
	}

	if !validHTTPToken(c.Header) {
		return normalizedConfig{}, fmt.Errorf("idempotency: header must be a valid HTTP field name")
	}
	backend := strings.ToLower(strings.TrimSpace(c.Backend))
	if backend != BackendMemory && backend != BackendRedis {
		return normalizedConfig{}, fmt.Errorf("idempotency: backend must be memory or redis")
	}
	if c.TTL <= 0 || c.PendingTTL <= 0 || c.OperationTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("idempotency: ttl, pending_ttl, and operation_timeout must be positive")
	}
	if c.PendingStatus != 409 && c.PendingStatus != 425 {
		return normalizedConfig{}, fmt.Errorf("idempotency: pending_status must be 409 or 425")
	}
	if c.MinKeyLength < 1 || c.MaxKeyLength < c.MinKeyLength || c.MaxKeyLength > 512 {
		return normalizedConfig{}, fmt.Errorf("idempotency: key length bounds are invalid")
	}
	if c.MaxRequestBytes <= 0 || c.MaxResponseBytes <= 0 {
		return normalizedConfig{}, fmt.Errorf("idempotency: body limits must be positive")
	}
	redisInstance := plugin.NormalizeInstance(strings.TrimSpace(c.RedisInstance))
	if err := plugin.ValidateInstanceName(redisInstance); err != nil {
		return normalizedConfig{}, fmt.Errorf("idempotency: invalid redis_instance: %w", err)
	}
	if backend == BackendRedis && (strings.TrimSpace(c.RedisPrefix) == "" || containsControl(c.RedisPrefix)) {
		return normalizedConfig{}, fmt.Errorf("idempotency: redis_prefix cannot be empty or contain control characters")
	}
	return normalizedConfig{
		header:           c.Header,
		backend:          backend,
		redisInstance:    redisInstance,
		redisPrefix:      c.RedisPrefix,
		ttl:              c.TTL,
		pendingTTL:       c.PendingTTL,
		operationTimeout: c.OperationTimeout,
		pendingStatus:    c.PendingStatus,
		minKeyLength:     c.MinKeyLength,
		maxKeyLength:     c.MaxKeyLength,
		maxRequestBytes:  c.MaxRequestBytes,
		maxResponseBytes: c.MaxResponseBytes,
	}, nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
