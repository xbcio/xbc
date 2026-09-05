package accesslog

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultRequestIDHeader = "X-Request-ID"

// Config is bound from plugins.accesslog. Paths are matched against the URL
// path only: an exact value matches one path, and a value ending in '*' is a
// prefix rule. Raw query strings and request/response headers are never logged.
type Config struct {
	SkipPaths         []string      `yaml:"skip_paths"`
	RequestIDHeader   string        `yaml:"request_id_header"    default:"X-Request-ID" validate:"required"`
	TrustProxyHeaders bool          `yaml:"trust_proxy_headers"  default:"false"`
	SlowRequest       time.Duration `yaml:"slow_request"         default:"0s" validate:"gte=0"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config { return Config{RequestIDHeader: defaultRequestIDHeader} }

// Validate checks all request-time settings.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type pathRule struct {
	value  string
	prefix bool
}

type normalizedConfig struct {
	skipPaths         []pathRule
	requestIDHeader   string
	trustProxyHeaders bool
	slowRequest       time.Duration
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	header := strings.TrimSpace(c.RequestIDHeader)
	if header == "" || !validHeaderName(header) {
		return normalizedConfig{}, fmt.Errorf("accesslog: request_id_header must be a valid HTTP header name")
	}
	if c.SlowRequest < 0 {
		return normalizedConfig{}, fmt.Errorf("accesslog: slow_request cannot be negative")
	}
	rules := make([]pathRule, 0, len(c.SkipPaths))
	for _, raw := range c.SkipPaths {
		value := strings.TrimSpace(raw)
		if value == "" || value[0] != '/' || strings.ContainsAny(value, "?#\r\n") {
			return normalizedConfig{}, fmt.Errorf("accesslog: invalid skip path %q", raw)
		}
		prefix := strings.HasSuffix(value, "*")
		if prefix {
			value = strings.TrimSuffix(value, "*")
		}
		if strings.Contains(value, "*") {
			return normalizedConfig{}, fmt.Errorf("accesslog: wildcard is only allowed at the end of skip path %q", raw)
		}
		rules = append(rules, pathRule{value: value, prefix: prefix})
	}
	return normalizedConfig{
		skipPaths:         rules,
		requestIDHeader:   http.CanonicalHeaderKey(header),
		trustProxyHeaders: c.TrustProxyHeaders,
		slowRequest:       c.SlowRequest,
	}, nil
}

func validHeaderName(value string) bool {
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))) {
			return false
		}
	}
	return value != ""
}
