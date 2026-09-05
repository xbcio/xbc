package health

import (
	"fmt"
	"strings"
	"time"
)

// Config is bound from plugins.health.
type Config struct {
	Timeout       time.Duration `yaml:"timeout"         default:"2s"      validate:"gt=0"`
	LivenessPath  string        `yaml:"liveness_path"   default:"/healthz" validate:"required,startswith=/"`
	ReadinessPath string        `yaml:"readiness_path"  default:"/readyz"  validate:"required,startswith=/,nefield=LivenessPath"`
	DetailPolicy  DetailPolicy  `yaml:"detail_policy"   default:"never"    validate:"oneof=never always"`
}

// DefaultConfig returns the same production-safe defaults applied by XBC's
// configuration binder.
func DefaultConfig() Config {
	return Config{
		Timeout:       2 * time.Second,
		LivenessPath:  "/healthz",
		ReadinessPath: "/readyz",
		DetailPolicy:  DetailNever,
	}
}

func (c Config) validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("health: timeout must be greater than zero")
	}
	if err := validateProbePath("liveness_path", c.LivenessPath); err != nil {
		return err
	}
	if err := validateProbePath("readiness_path", c.ReadinessPath); err != nil {
		return err
	}
	if c.LivenessPath == c.ReadinessPath {
		return fmt.Errorf("health: liveness_path and readiness_path must differ")
	}
	if c.DetailPolicy != DetailNever && c.DetailPolicy != DetailAlways {
		return fmt.Errorf("health: detail_policy must be %q or %q", DetailNever, DetailAlways)
	}
	return nil
}

func validateProbePath(field, value string) error {
	if value == "" || value[0] != '/' {
		return fmt.Errorf("health: %s must be an absolute HTTP path", field)
	}
	if strings.ContainsAny(value, "?#") {
		return fmt.Errorf("health: %s must not contain a query or fragment", field)
	}
	return nil
}
