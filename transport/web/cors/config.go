package cors

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const defaultMaxAge = 12 * time.Hour

// Config is bound from plugins.cors. AllowOrigins accepts exact serialized
// origins and the standalone wildcard "*". Wildcard origins and credentials
// are intentionally mutually exclusive: reflecting arbitrary origins while
// allowing credentials would silently turn a typo into a cross-site data leak.
type Config struct {
	AllowOrigins     []string      `yaml:"allow_origins"      default:"*"                                      validate:"min=1,dive,required"`
	AllowMethods     []string      `yaml:"allow_methods"      default:"GET,POST,PUT,PATCH,DELETE,HEAD,OPTIONS" validate:"min=1,dive,required"`
	AllowHeaders     []string      `yaml:"allow_headers"      default:"Origin,Content-Type,Accept,Authorization"`
	ExposeHeaders    []string      `yaml:"expose_headers"`
	AllowCredentials bool          `yaml:"allow_credentials" default:"false"`
	MaxAge           time.Duration `yaml:"max_age"            default:"12h"                                    validate:"gte=0"`
}

// DefaultConfig returns the same defaults applied by XBC's configuration
// binder. It is useful when constructing the plugin directly with New.
func DefaultConfig() Config {
	return Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization"},
		MaxAge:       defaultMaxAge,
	}
}

// Validate checks semantic constraints that cannot be expressed by struct
// validation tags. XBC also runs its normal tag validation before Plugin.Init.
func (c Config) Validate() error {
	_, err := compilePolicy(c)
	return err
}

func normalizeOrigins(values []string) (map[string]struct{}, bool, error) {
	if len(values) == 0 {
		return nil, false, fmt.Errorf("cors: allow_origins must contain at least one origin")
	}

	origins := make(map[string]struct{}, len(values))
	wildcard := false
	for _, raw := range values {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			return nil, false, fmt.Errorf("cors: allow_origins cannot contain an empty origin")
		}
		if containsControl(origin) {
			return nil, false, fmt.Errorf("cors: allow_origins contains an invalid origin %q", raw)
		}
		if origin == "*" {
			wildcard = true
			continue
		}
		if strings.Contains(origin, "*") {
			return nil, false, fmt.Errorf("cors: allow_origins only supports * as a complete wildcard, got %q", raw)
		}
		if origin != "null" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return nil, false, fmt.Errorf("cors: allow_origins contains invalid serialized origin %q", raw)
			}
		}
		origins[origin] = struct{}{}
	}
	if wildcard && len(origins) != 0 {
		return nil, false, fmt.Errorf("cors: wildcard allow origin must be the only allow_origins entry")
	}
	return origins, wildcard, nil
}

func normalizeMethods(values []string) ([]string, map[string]struct{}, bool, error) {
	if len(values) == 0 {
		return nil, nil, false, fmt.Errorf("cors: allow_methods must contain at least one method")
	}
	ordered := make([]string, 0, len(values))
	allowed := make(map[string]struct{}, len(values))
	wildcard := false
	for _, raw := range values {
		method := strings.TrimSpace(raw)
		if !isToken(method) {
			return nil, nil, false, fmt.Errorf("cors: allow_methods contains invalid HTTP method %q", raw)
		}
		if method == "*" {
			wildcard = true
			continue
		}
		if _, exists := allowed[method]; exists {
			continue
		}
		allowed[method] = struct{}{}
		ordered = append(ordered, method)
	}
	if wildcard && len(ordered) != 0 {
		return nil, nil, false, fmt.Errorf("cors: wildcard must be the only allow_methods entry")
	}
	if wildcard {
		ordered = []string{"*"}
	}
	return ordered, allowed, wildcard, nil
}

func normalizeHeaders(field string, values []string, allowWildcard bool) ([]string, map[string]struct{}, bool, error) {
	ordered := make([]string, 0, len(values))
	allowed := make(map[string]struct{}, len(values))
	wildcard := false
	for _, raw := range values {
		header := strings.TrimSpace(raw)
		if header == "*" && allowWildcard {
			wildcard = true
			continue
		}
		if !isToken(header) {
			return nil, nil, false, fmt.Errorf("cors: %s contains invalid HTTP header name %q", field, raw)
		}
		key := strings.ToLower(header)
		if _, exists := allowed[key]; exists {
			continue
		}
		allowed[key] = struct{}{}
		ordered = append(ordered, canonicalHeaderName(header))
	}
	if wildcard && len(ordered) != 0 {
		return nil, nil, false, fmt.Errorf("cors: wildcard must be the only %s entry", field)
	}
	if wildcard {
		ordered = []string{"*"}
	}
	return ordered, allowed, wildcard, nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r <= 0x1f || r == 0x7f {
			return true
		}
	}
	return false
}

// isToken implements RFC 9110's tchar production. Keeping this local avoids a
// dependency merely for validating a small, security-sensitive grammar.
func isToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
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

func canonicalHeaderName(value string) string {
	parts := strings.Split(strings.ToLower(value), "-")
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "-")
}
