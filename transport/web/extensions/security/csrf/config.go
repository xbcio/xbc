package csrf

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

const (
	defaultMode          = "origin"
	defaultHeader        = "X-CSRF-Token"
	defaultSessionCookie = "xbc_session"
	minSecretBytes       = 32
)

// Config is bound from plugins.csrf. Mode selects the protection strategy:
// "origin" validates the request's Origin or Referer and an optional explicit
// origin allowlist; "token" requires an HMAC token bound to the session cookie.
// AllowOrigins accepts absolute http or https origins only -- a wildcard is
// deliberately refused, because accepting arbitrary origins is exactly the
// cross-site request the middleware exists to stop. Secret is required in token
// mode and has no default.
type Config struct {
	Mode          string   `yaml:"mode" default:"origin" validate:"omitempty,oneof=origin token"`
	AllowOrigins  []string `yaml:"allow_origins"`
	ExcludePaths  []string `yaml:"exclude_paths"`
	ExcludeRoutes []string `yaml:"exclude_routes"`
	Header        string   `yaml:"header" default:"X-CSRF-Token" validate:"required"`
	SessionCookie string   `yaml:"session_cookie" default:"xbc_session" validate:"required"`
	Secret        string   `yaml:"secret" mask:"true"`
}

// DefaultConfig returns the same defaults applied by XBC's configuration
// binder. It is useful when constructing the plugin directly with New.
func DefaultConfig() Config {
	return Config{
		Mode:          defaultMode,
		Header:        defaultHeader,
		SessionCookie: defaultSessionCookie,
	}
}

// Validate checks semantic constraints that cannot be expressed by struct
// validation tags. XBC also runs its normal tag validation before Plugin.Init.
func (c Config) Validate() error {
	_, err := compilePolicy(c)
	return err
}

func parseMode(value string) (mode, error) {
	switch value {
	case "", "origin":
		return modeOrigin, nil
	case "token":
		return modeToken, nil
	default:
		return modeOrigin, fmt.Errorf("csrf: mode must be \"origin\" or \"token\", got %q", value)
	}
}

// normalizeAllowOrigins validates every entry as an absolute http or https
// origin and returns the exact serialized origins, with a trailing slash
// removed so "https://app.example.com/" and "https://app.example.com" name the
// same origin. Wildcards are refused: an origin allowlist that matches every
// origin is not a CSRF policy.
func normalizeAllowOrigins(values []string) (map[string]struct{}, error) {
	if len(values) == 0 {
		return nil, nil
	}
	origins := make(map[string]struct{}, len(values))
	for _, raw := range values {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			return nil, fmt.Errorf("csrf: allow_origins cannot contain an empty origin")
		}
		if origin == "*" {
			return nil, fmt.Errorf("csrf: allow_origins must list explicit origins; the wildcard %q would accept every cross-site request", origin)
		}
		if strings.Contains(origin, "*") {
			return nil, fmt.Errorf("csrf: allow_origins entry %q is not an absolute http or https origin; wildcards are not supported", raw)
		}
		parsed, err := url.Parse(origin)
		if err != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" ||
			parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") ||
			parsed.RawQuery != "" ||
			parsed.Fragment != "" {
			return nil, fmt.Errorf("csrf: allow_origins entry %q must be an absolute http or https origin such as https://app.example.com", raw)
		}
		origins[strings.TrimSuffix(origin, "/")] = struct{}{}
	}
	return origins, nil
}

func validateExcludePaths(values []string) error {
	for _, pattern := range values {
		if pattern == "" {
			return fmt.Errorf("csrf: exclude_paths cannot contain an empty pattern")
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("csrf: exclude_paths entry %q is not a valid path pattern", pattern)
		}
	}
	return nil
}

func normalizeExcludeRoutes(values []string) (map[string]struct{}, error) {
	if len(values) == 0 {
		return nil, nil
	}
	routes := make(map[string]struct{}, len(values))
	for _, name := range values {
		if name == "" {
			return nil, fmt.Errorf("csrf: exclude_routes cannot contain an empty route name")
		}
		routes[name] = struct{}{}
	}
	return routes, nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r <= 0x1f || r == 0x7f {
			return true
		}
	}
	return false
}

// isToken implements RFC 9110's tchar production, which is also RFC 6265's
// cookie-name grammar. Keeping this local avoids a dependency merely for
// validating a small, security-sensitive grammar.
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
