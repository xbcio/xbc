package health

import (
	"fmt"
	"strings"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
)

// Config is bound from plugins.health-http. It carries only what the HTTP
// surface owns: where the probes are served, and whether a diagnostic error may
// cross the protocol boundary. The per-check timeout is probe policy and stays
// with the neutral capability at plugins.health.
type Config struct {
	LivenessPath  string                  `yaml:"liveness_path"   default:"/healthz" validate:"required,startswith=/"`
	ReadinessPath string                  `yaml:"readiness_path"  default:"/readyz"  validate:"required,startswith=/,nefield=LivenessPath"`
	DetailPolicy  corehealth.DetailPolicy `yaml:"detail_policy"   default:"never"    validate:"oneof=never always"`
}

// DefaultConfig returns the same production-safe defaults applied by XBC's
// configuration binder.
func DefaultConfig() Config {
	return Config{
		LivenessPath:  "/healthz",
		ReadinessPath: "/readyz",
		DetailPolicy:  corehealth.DetailNever,
	}
}

func (c Config) validate() error {
	if err := validateProbePath("liveness_path", c.LivenessPath); err != nil {
		return err
	}
	if err := validateProbePath("readiness_path", c.ReadinessPath); err != nil {
		return err
	}
	if c.LivenessPath == c.ReadinessPath {
		return fmt.Errorf("health-http: liveness_path and readiness_path must differ")
	}
	if c.DetailPolicy != corehealth.DetailNever && c.DetailPolicy != corehealth.DetailAlways {
		return fmt.Errorf("health-http: detail_policy must be %q or %q", corehealth.DetailNever, corehealth.DetailAlways)
	}
	return nil
}

func validateProbePath(field, value string) error {
	if value == "" || value[0] != '/' {
		return fmt.Errorf("health-http: %s must be an absolute HTTP path", field)
	}
	if strings.ContainsAny(value, "?#") {
		return fmt.Errorf("health-http: %s must not contain a query or fragment", field)
	}
	return nil
}
