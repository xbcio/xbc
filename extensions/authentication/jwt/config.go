package jwt

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultAlgorithm = "HS256"
	defaultHeader    = "Authorization"
	defaultScheme    = "Bearer"
	defaultExpire    = 2 * time.Hour
)

var defaultAlgorithms = []string{"HS256", "HS384", "HS512"}

// Config is bound from plugins.jwt. Secret has no default and must contain at
// least 32 bytes. Algorithm selects the signing method; Algorithms is the
// verification allowlist and is restricted to the HMAC SHA-2 family so a
// symmetric secret can never be confused with an asymmetric verification key.
type Config struct {
	Secret     string        `yaml:"secret" validate:"required,min=32" mask:"true"`
	Algorithm  string        `yaml:"algorithm"          default:"HS256"`
	Algorithms []string      `yaml:"algorithms"         default:"HS256,HS384,HS512"`
	Issuer     string        `yaml:"issuer"`
	Audience   []string      `yaml:"audience"`
	Leeway     time.Duration `yaml:"leeway"             default:"0s"`
	Header     string        `yaml:"header"             default:"Authorization"`
	Scheme     string        `yaml:"scheme"             default:"Bearer"`
	Expire     time.Duration `yaml:"expire"             default:"2h"`
}

// Validate checks security-sensitive invariants without exposing Secret.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type normalizedConfig struct {
	secret     []byte
	algorithm  string
	algorithms []string
	allowed    map[string]struct{}
	issuer     string
	audience   []string
	leeway     time.Duration
	header     string
	scheme     string
	expire     time.Duration
}

// DefaultConfig returns JWT's default configuration. Secret has no default
// and must be supplied through plugins.jwt.secret before use.
func DefaultConfig() Config {
	return Config{
		Algorithm:  defaultAlgorithm,
		Algorithms: append([]string(nil), defaultAlgorithms...),
		Header:     defaultHeader,
		Scheme:     defaultScheme,
		Expire:     defaultExpire,
	}
}

func prepareConfig(cfg Config) (Config, error) {
	if _, err := normalizeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	if len([]byte(c.Secret)) < 32 {
		return normalizedConfig{}, fmt.Errorf("jwt: secret must be at least 32 bytes")
	}

	algorithm := strings.TrimSpace(c.Algorithm)
	if algorithm == "" {
		algorithm = defaultAlgorithm
	}

	algorithms := c.Algorithms
	if algorithms == nil {
		algorithms = defaultAlgorithms
	}
	if len(algorithms) == 0 {
		return normalizedConfig{}, fmt.Errorf("jwt: algorithms allowlist cannot be empty")
	}
	allowed := make(map[string]struct{}, len(algorithms))
	normalizedAlgorithms := make([]string, 0, len(algorithms))
	for _, candidate := range algorithms {
		candidate = strings.TrimSpace(candidate)
		if !isSupportedAlgorithm(candidate) {
			return normalizedConfig{}, fmt.Errorf("jwt: unsupported algorithm %q; allowed algorithms are HS256, HS384, and HS512", candidate)
		}
		if _, duplicate := allowed[candidate]; duplicate {
			continue
		}
		allowed[candidate] = struct{}{}
		normalizedAlgorithms = append(normalizedAlgorithms, candidate)
	}
	if !isSupportedAlgorithm(algorithm) {
		return normalizedConfig{}, fmt.Errorf("jwt: unsupported signing algorithm %q; allowed algorithms are HS256, HS384, and HS512", algorithm)
	}
	if _, ok := allowed[algorithm]; !ok {
		return normalizedConfig{}, fmt.Errorf("jwt: signing algorithm %q is not in the algorithms allowlist", algorithm)
	}

	if c.Leeway < 0 {
		return normalizedConfig{}, fmt.Errorf("jwt: leeway cannot be negative")
	}
	expire := c.Expire
	if expire == 0 {
		expire = defaultExpire
	}
	if expire < 0 {
		return normalizedConfig{}, fmt.Errorf("jwt: expire must be greater than zero")
	}

	header := strings.TrimSpace(c.Header)
	if header == "" {
		header = defaultHeader
	}
	if !validToken(header) {
		return normalizedConfig{}, fmt.Errorf("jwt: header must be a valid HTTP header name")
	}
	scheme := strings.TrimSpace(c.Scheme)
	if scheme == "" {
		scheme = defaultScheme
	}
	if !validToken(scheme) {
		return normalizedConfig{}, fmt.Errorf("jwt: scheme must be a valid HTTP authentication scheme")
	}

	audience := make([]string, 0, len(c.Audience))
	seenAudience := make(map[string]struct{}, len(c.Audience))
	for _, value := range c.Audience {
		value = strings.TrimSpace(value)
		if value == "" {
			return normalizedConfig{}, fmt.Errorf("jwt: audience values cannot be empty")
		}
		if _, duplicate := seenAudience[value]; duplicate {
			continue
		}
		seenAudience[value] = struct{}{}
		audience = append(audience, value)
	}

	return normalizedConfig{
		secret:     append([]byte(nil), []byte(c.Secret)...),
		algorithm:  algorithm,
		algorithms: normalizedAlgorithms,
		allowed:    allowed,
		issuer:     c.Issuer,
		audience:   audience,
		leeway:     c.Leeway,
		header:     header,
		scheme:     scheme,
		expire:     expire,
	}, nil
}

func isSupportedAlgorithm(algorithm string) bool {
	switch algorithm {
	case "HS256", "HS384", "HS512":
		return true
	default:
		return false
	}
}

// validToken implements the RFC 7230 token character set used by both HTTP
// header names and authentication schemes.
func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}
