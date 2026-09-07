package timeout

import (
	"fmt"
	"strings"
	"time"
)

const defaultDuration = 15 * time.Second

// Config is bound from plugins.timeout. ExcludePaths supports exact paths and
// trailing-* prefix rules. ExcludeRoutes matches web.RouteInfo.Name exactly.
type Config struct {
	Duration      time.Duration `yaml:"duration"       default:"15s" validate:"gt=0"`
	ExcludePaths  []string      `yaml:"exclude_paths"`
	ExcludeRoutes []string      `yaml:"exclude_routes"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config { return Config{Duration: defaultDuration} }

// Validate checks deadline and bypass selectors.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type pathRule struct {
	value  string
	prefix bool
}

type normalizedConfig struct {
	duration      time.Duration
	excludePaths  []pathRule
	excludeRoutes map[string]struct{}
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	if c.Duration <= 0 {
		return normalizedConfig{}, fmt.Errorf("timeout: duration must be greater than zero")
	}
	paths := make([]pathRule, 0, len(c.ExcludePaths))
	for _, raw := range c.ExcludePaths {
		value := strings.TrimSpace(raw)
		if value == "" || value[0] != '/' || strings.ContainsAny(value, "?#\r\n") {
			return normalizedConfig{}, fmt.Errorf("timeout: invalid excluded path %q", raw)
		}
		prefix := strings.HasSuffix(value, "*")
		if prefix {
			value = strings.TrimSuffix(value, "*")
		}
		if strings.Contains(value, "*") {
			return normalizedConfig{}, fmt.Errorf("timeout: wildcard is only allowed at the end of excluded path %q", raw)
		}
		paths = append(paths, pathRule{value: value, prefix: prefix})
	}
	routes := make(map[string]struct{}, len(c.ExcludeRoutes))
	for _, raw := range c.ExcludeRoutes {
		value := strings.TrimSpace(raw)
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return normalizedConfig{}, fmt.Errorf("timeout: exclude_routes cannot contain an empty or malformed name")
		}
		routes[value] = struct{}{}
	}
	return normalizedConfig{duration: c.Duration, excludePaths: paths, excludeRoutes: routes}, nil
}

func (c normalizedConfig) excludedPath(path string) bool {
	for _, rule := range c.excludePaths {
		if (!rule.prefix && path == rule.value) || (rule.prefix && strings.HasPrefix(path, rule.value)) {
			return true
		}
	}
	return false
}
