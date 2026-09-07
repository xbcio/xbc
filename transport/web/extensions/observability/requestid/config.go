package requestid

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultHeader    = "X-Request-ID"
	defaultMaxLength = 128
)

// Config is bound from plugins.requestid.
type Config struct {
	Header        string `yaml:"header"         default:"X-Request-ID" validate:"required"`
	TrustIncoming bool   `yaml:"trust_incoming" default:"true"`
	MaxLength     int    `yaml:"max_length"     default:"128" validate:"min=16,max=1024"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{Header: defaultHeader, TrustIncoming: true, MaxLength: defaultMaxLength}
}

// Validate checks limits and prevents response-header injection.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type normalizedConfig struct {
	header        string
	trustIncoming bool
	maxLength     int
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	header := strings.TrimSpace(c.Header)
	if header == "" {
		return normalizedConfig{}, fmt.Errorf("requestid: header is required")
	}
	if !validToken(header) {
		return normalizedConfig{}, fmt.Errorf("requestid: header must be a valid HTTP header name")
	}
	if c.MaxLength < 16 || c.MaxLength > 1024 {
		return normalizedConfig{}, fmt.Errorf("requestid: max_length must be between 16 and 1024")
	}
	return normalizedConfig{
		header:        http.CanonicalHeaderKey(header),
		trustIncoming: c.TrustIncoming,
		maxLength:     c.MaxLength,
	}, nil
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))) {
			return false
		}
	}
	return true
}
