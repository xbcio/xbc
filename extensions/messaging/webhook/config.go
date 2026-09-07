package webhook

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	BackpressureBlock  = "block"
	BackpressureReject = "reject"
)

// Config controls one named webhook client.
type Config struct {
	Workers          int           `yaml:"workers" default:"4"`
	QueueSize        int           `yaml:"queue_size" default:"1024"`
	Backpressure     string        `yaml:"backpressure" default:"block"`
	MaxAttempts      int           `yaml:"max_attempts" default:"5"`
	RequestTimeout   time.Duration `yaml:"request_timeout" default:"10s"`
	MaxPayloadBytes  int           `yaml:"max_payload_bytes" default:"1048576"`
	MaxResponseBytes int           `yaml:"max_response_bytes" default:"65536"`
	AllowHTTP        bool          `yaml:"allow_http" default:"false"`
	MaxRedirects     int           `yaml:"max_redirects" default:"5"`
	InitialBackoff   time.Duration `yaml:"initial_backoff" default:"500ms"`
	MaxBackoff       time.Duration `yaml:"max_backoff" default:"30s"`
	Jitter           float64       `yaml:"jitter" default:"0.2"`
	MaxRetryAfter    time.Duration `yaml:"max_retry_after" default:"30s"`
}

// DefaultConfig returns the same defaults used by New.
func DefaultConfig() Config {
	return Config{
		Workers:          4,
		QueueSize:        1024,
		Backpressure:     BackpressureBlock,
		MaxAttempts:      5,
		RequestTimeout:   10 * time.Second,
		MaxPayloadBytes:  1 << 20,
		MaxResponseBytes: 64 << 10,
		MaxRedirects:     5,
		InitialBackoff:   500 * time.Millisecond,
		MaxBackoff:       30 * time.Second,
		Jitter:           0.2,
		MaxRetryAfter:    30 * time.Second,
	}
}

func defaultConfig() Config { return DefaultConfig() }

func (c Config) normalized() (Config, error) {
	d := DefaultConfig()
	if c.Workers == 0 {
		c.Workers = d.Workers
	}
	if c.QueueSize == 0 {
		c.QueueSize = d.QueueSize
	}
	if strings.TrimSpace(c.Backpressure) == "" {
		c.Backpressure = d.Backpressure
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.MaxPayloadBytes == 0 {
		c.MaxPayloadBytes = d.MaxPayloadBytes
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = d.MaxResponseBytes
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = d.InitialBackoff
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = d.MaxBackoff
	}
	// MaxRedirects, Jitter, and MaxRetryAfter deliberately retain explicit
	// zero values: zero disables redirects, jitter, or Retry-After waiting.
	c.Backpressure = strings.ToLower(strings.TrimSpace(c.Backpressure))

	if c.Workers <= 0 || c.QueueSize <= 0 || c.MaxAttempts <= 0 || c.MaxRedirects < 0 {
		return Config{}, errors.New("webhook: workers, queue_size, max_attempts must be positive and max_redirects non-negative")
	}
	if c.RequestTimeout <= 0 || c.InitialBackoff <= 0 || c.MaxBackoff <= 0 || c.MaxRetryAfter < 0 {
		return Config{}, errors.New("webhook: timeout and backoff values are invalid")
	}
	if c.MaxPayloadBytes <= 0 || c.MaxResponseBytes <= 0 {
		return Config{}, errors.New("webhook: payload and response limits must be positive")
	}
	if c.MaxBackoff < c.InitialBackoff {
		return Config{}, errors.New("webhook: max_backoff must not be less than initial_backoff")
	}
	if c.Jitter < 0 || c.Jitter > 1 {
		return Config{}, errors.New("webhook: jitter must be between 0 and 1")
	}
	switch c.Backpressure {
	case BackpressureBlock, BackpressureReject:
	default:
		return Config{}, fmt.Errorf("webhook: unsupported backpressure %q", c.Backpressure)
	}
	return c, nil
}

// Validate checks configuration without network I/O.
func (c Config) Validate() error {
	_, err := c.normalized()
	return err
}
