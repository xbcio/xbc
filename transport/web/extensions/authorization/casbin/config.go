package casbin

import "fmt"

// MissingPermissionPolicy controls a protected route without RouteInfo.Perm
// when the enforcer's request convention is route_permission.
type MissingPermissionPolicy string

const (
	// MissingPermissionDeny is the secure default: metadata omissions cannot
	// accidentally publish an authenticated but unauthorized endpoint.
	MissingPermissionDeny MissingPermissionPolicy = "deny"
	// MissingPermissionAllow skips Casbin for the route, but still requires a
	// verified subject. It must be selected explicitly.
	MissingPermissionAllow MissingPermissionPolicy = "allow"
)

// Config is bound from plugins.casbin-http.
//
// The request convention is deliberately not a field here. It selects the
// neutral engine's model and policy shape and is configured and validated as
// plugins.casbin.request_convention; this middleware reads it back from the
// enforcer's own request definition (see requestConventionOf), so a
// configuration that disagrees with the model it was built for cannot be
// expressed.
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
		return normalizedConfig{}, fmt.Errorf("casbin-http: missing_permission must be %q or %q, got %q", MissingPermissionDeny, MissingPermissionAllow, missing)
	}
	return normalizedConfig{missingPermission: missing}, nil
}
