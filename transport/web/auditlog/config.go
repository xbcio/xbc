package auditlog

import (
	"fmt"
	pathpkg "path"
	"strings"
	"time"
)

const (
	OverflowDropNewest = "drop_newest"
	OverflowBlock      = "block"
)

// Config is bound from plugins.auditlog. Bodies are intentionally not
// configurable: this plugin never records them.
type Config struct {
	Async             bool          `yaml:"async"`
	QueueSize         int           `yaml:"queue_size"          default:"1024" validate:"min=1,max=1048576"`
	Overflow          string        `yaml:"overflow"            default:"drop_newest"`
	SinkTimeout       time.Duration `yaml:"sink_timeout"        default:"2s" validate:"gt=0"`
	RequestIDHeader   string        `yaml:"request_id_header"   default:"X-Request-ID"`
	IdempotencyHeader string        `yaml:"idempotency_header" default:"Idempotency-Key"`
	SkipPaths         []string      `yaml:"skip_paths"`
}

type normalizedConfig struct {
	async             bool
	queueSize         int
	overflow          string
	sinkTimeout       time.Duration
	requestIDHeader   string
	idempotencyHeader string
	skipPaths         []string
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		QueueSize:         1024,
		Overflow:          OverflowDropNewest,
		SinkTimeout:       2 * time.Second,
		RequestIDHeader:   "X-Request-ID",
		IdempotencyHeader: "Idempotency-Key",
	}
}

// Validate checks semantic configuration invariants.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := DefaultConfig()
	if c.QueueSize == 0 {
		c.QueueSize = d.QueueSize
	}
	if strings.TrimSpace(c.Overflow) == "" {
		c.Overflow = d.Overflow
	}
	if c.SinkTimeout == 0 {
		c.SinkTimeout = d.SinkTimeout
	}
	if strings.TrimSpace(c.RequestIDHeader) == "" {
		c.RequestIDHeader = d.RequestIDHeader
	}
	if strings.TrimSpace(c.IdempotencyHeader) == "" {
		c.IdempotencyHeader = d.IdempotencyHeader
	}
	if c.QueueSize < 1 || c.QueueSize > 1<<20 {
		return normalizedConfig{}, fmt.Errorf("auditlog: queue_size must be between 1 and 1048576")
	}
	overflow := strings.ToLower(strings.TrimSpace(c.Overflow))
	if overflow != OverflowDropNewest && overflow != OverflowBlock {
		return normalizedConfig{}, fmt.Errorf("auditlog: overflow must be drop_newest or block")
	}
	if c.SinkTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("auditlog: sink_timeout must be positive")
	}
	if !validToken(c.RequestIDHeader) || !validToken(c.IdempotencyHeader) {
		return normalizedConfig{}, fmt.Errorf("auditlog: request and idempotency headers must be valid HTTP field names")
	}
	patterns := make([]string, 0, len(c.SkipPaths))
	seen := make(map[string]struct{}, len(c.SkipPaths))
	for _, raw := range c.SkipPaths {
		pattern := strings.TrimSpace(raw)
		if pattern == "" || !strings.HasPrefix(pattern, "/") {
			return normalizedConfig{}, fmt.Errorf("auditlog: skip path %q must start with /", raw)
		}
		if _, err := pathpkg.Match(pattern, "/validation"); err != nil {
			return normalizedConfig{}, fmt.Errorf("auditlog: invalid skip path pattern %q: %w", raw, err)
		}
		if _, duplicate := seen[pattern]; duplicate {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	return normalizedConfig{
		async:             c.Async,
		queueSize:         c.QueueSize,
		overflow:          overflow,
		sinkTimeout:       c.SinkTimeout,
		requestIDHeader:   c.RequestIDHeader,
		idempotencyHeader: c.IdempotencyHeader,
		skipPaths:         patterns,
	}, nil
}

func validToken(value string) bool {
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
