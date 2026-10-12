package accesslog

import (
	"fmt"
	"strings"
	"time"
)

// Config is bound from plugins.grpc-accesslog. Skip rules are matched against
// the full method name: an exact value such as
// "/grpc.health.v1.Health/Check" matches one method, and a value ending in
// '*' such as "/grpc.health.v1.Health/*" is a prefix rule.
type Config struct {
	SkipMethods []string      `yaml:"skip_methods"`
	SlowCall    time.Duration `yaml:"slow_call" default:"0s" validate:"gte=0"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config { return Config{} }

// Validate checks every call-time setting.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type methodRule struct {
	value  string
	prefix bool
}

type normalizedConfig struct {
	skipMethods []methodRule
	slowCall    time.Duration
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	if c.SlowCall < 0 {
		return normalizedConfig{}, fmt.Errorf("grpc-accesslog: slow_call cannot be negative")
	}
	rules := make([]methodRule, 0, len(c.SkipMethods))
	for _, raw := range c.SkipMethods {
		value := strings.TrimSpace(raw)
		if value == "" || value[0] != '/' || strings.ContainsAny(value, " \t\r\n") {
			return normalizedConfig{}, fmt.Errorf("grpc-accesslog: invalid skip method %q", raw)
		}
		prefix := strings.HasSuffix(value, "*")
		if prefix {
			value = strings.TrimSuffix(value, "*")
		}
		if strings.Contains(value, "*") {
			return normalizedConfig{}, fmt.Errorf("grpc-accesslog: wildcard is only allowed at the end of skip method %q", raw)
		}
		rules = append(rules, methodRule{value: value, prefix: prefix})
	}
	return normalizedConfig{skipMethods: rules, slowCall: c.SlowCall}, nil
}

func (c normalizedConfig) skip(fullMethod string) bool {
	for _, rule := range c.skipMethods {
		if (!rule.prefix && fullMethod == rule.value) || (rule.prefix && strings.HasPrefix(fullMethod, rule.value)) {
			return true
		}
	}
	return false
}
