package casbin

import (
	"fmt"
	"strings"
	"time"
)

// RequestConvention selects the Casbin request tuple produced for a route.
type RequestConvention string

const (
	// ConventionRoutePermission calls Enforce(subject, route.Perm). It is the
	// default and treats an empty permission according to MissingPermission.
	ConventionRoutePermission RequestConvention = "route_permission"
	// ConventionPathMethod calls Enforce(subject, route.Path, route.Method).
	// Paths are route templates (for example /users/:id), not user input.
	ConventionPathMethod RequestConvention = "path_method"
)

// MissingPermissionPolicy controls a protected route without RouteInfo.Perm
// when ConventionRoutePermission is selected.
type MissingPermissionPolicy string

const (
	// MissingPermissionDeny is the secure default: metadata omissions cannot
	// accidentally publish an authenticated but unauthorized endpoint.
	MissingPermissionDeny MissingPermissionPolicy = "deny"
	// MissingPermissionAllow skips Casbin for the route, but still requires a
	// verified subject. It must be selected explicitly.
	MissingPermissionAllow MissingPermissionPolicy = "allow"
)

// Config is bound from plugins.casbin.
//
// Model and ModelFile are mutually exclusive; when neither is set, a secure
// built-in model matching RequestConvention is used. Policy and PolicyFile
// are also mutually exclusive; neither means an empty, deny-all policy.
// ReloadInterval periodically reloads PolicyFile. LoadPolicy remains available
// for synchronous programmatic reloads regardless of the source.
type Config struct {
	Model             string                  `yaml:"model"`
	ModelFile         string                  `yaml:"model_file"`
	Policy            string                  `yaml:"policy"`
	PolicyFile        string                  `yaml:"policy_file"`
	RequestConvention RequestConvention       `yaml:"request_convention" default:"route_permission" validate:"oneof=route_permission path_method"`
	MissingPermission MissingPermissionPolicy `yaml:"missing_permission" default:"deny" validate:"oneof=deny allow"`
	ReloadInterval    time.Duration           `yaml:"reload_interval" default:"0s" validate:"gte=0"`
}

// DefaultConfig returns the secure defaults used by New and configuration
// normalization.
func DefaultConfig() Config {
	return Config{
		RequestConvention: ConventionRoutePermission,
		MissingPermission: MissingPermissionDeny,
	}
}

// Validate checks source selection and request-policy settings without doing
// filesystem I/O. File readability and model/policy syntax are checked when
// the enforcer is built during construction.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// performing filesystem I/O and returns it unchanged.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

type normalizedConfig struct {
	model             string
	modelFile         string
	policy            string
	policyFile        string
	requestConvention RequestConvention
	missingPermission MissingPermissionPolicy
	reloadInterval    time.Duration
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	convention := c.RequestConvention
	if convention == "" {
		convention = ConventionRoutePermission
	}
	switch convention {
	case ConventionRoutePermission, ConventionPathMethod:
	default:
		return normalizedConfig{}, fmt.Errorf("casbin: request_convention must be %q or %q, got %q", ConventionRoutePermission, ConventionPathMethod, convention)
	}

	missing := c.MissingPermission
	if missing == "" {
		missing = MissingPermissionDeny
	}
	switch missing {
	case MissingPermissionDeny, MissingPermissionAllow:
	default:
		return normalizedConfig{}, fmt.Errorf("casbin: missing_permission must be %q or %q, got %q", MissingPermissionDeny, MissingPermissionAllow, missing)
	}

	modelSet := strings.TrimSpace(c.Model) != ""
	modelFile := strings.TrimSpace(c.ModelFile)
	if modelSet && modelFile != "" {
		return normalizedConfig{}, fmt.Errorf("casbin: model and model_file are mutually exclusive")
	}
	policySet := strings.TrimSpace(c.Policy) != ""
	policyFile := strings.TrimSpace(c.PolicyFile)
	if policySet && policyFile != "" {
		return normalizedConfig{}, fmt.Errorf("casbin: policy and policy_file are mutually exclusive")
	}
	if c.ReloadInterval < 0 {
		return normalizedConfig{}, fmt.Errorf("casbin: reload_interval cannot be negative")
	}
	if c.ReloadInterval > 0 && policyFile == "" {
		return normalizedConfig{}, fmt.Errorf("casbin: reload_interval requires policy_file")
	}

	return normalizedConfig{
		model:             c.Model,
		modelFile:         modelFile,
		policy:            c.Policy,
		policyFile:        policyFile,
		requestConvention: convention,
		missingPermission: missing,
		reloadInterval:    c.ReloadInterval,
	}, nil
}
