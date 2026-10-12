package casbin

import "fmt"

// MissingPermissionPolicy controls a method without a declared permission when
// the enforcer's request convention is route_permission.
type MissingPermissionPolicy string

const (
	// MissingPermissionDeny is the secure default: a method whose permission
	// declaration was omitted cannot accidentally become authorized.
	MissingPermissionDeny MissingPermissionPolicy = "deny"
	// MissingPermissionAllow skips Casbin for the method, but still requires a
	// verified subject. It must be selected explicitly.
	MissingPermissionAllow MissingPermissionPolicy = "allow"
)

// Config is bound from plugins.casbin-grpc.
//
// The request convention is deliberately not a field here. It selects the
// neutral engine's model and policy shape and is configured and validated as
// plugins.casbin.request_convention; this interceptor reads it back from the
// engine's own request definition at construction, so a configuration that
// disagrees with the model it was built for cannot be expressed.
type Config struct {
	MissingPermission MissingPermissionPolicy `yaml:"missing_permission" default:"deny" validate:"oneof=deny allow"`
}

// DefaultConfig returns the secure defaults used by New and configuration
// normalization.
func DefaultConfig() Config {
	return Config{
		MissingPermission: MissingPermissionDeny,
	}
}

// Validate checks the request policy without acquiring resources.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

type normalizedConfig struct {
	missingPermission MissingPermissionPolicy
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	missing := c.MissingPermission
	if missing == "" {
		missing = MissingPermissionDeny
	}
	switch missing {
	case MissingPermissionDeny, MissingPermissionAllow:
	default:
		return normalizedConfig{}, fmt.Errorf("casbin-grpc: missing_permission must be %q or %q, got %q", MissingPermissionDeny, MissingPermissionAllow, missing)
	}
	return normalizedConfig{missingPermission: missing}, nil
}
