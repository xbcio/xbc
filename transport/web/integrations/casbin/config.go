package casbin

import (
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/plugin"
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
// built-in model matching RequestConvention is used. Adapter selects a
// persistent policy provider and is mutually exclusive with Policy and
// PolicyFile. Without Adapter, Policy and PolicyFile are mutually exclusive
// and neither means an empty, deny-all policy. Watcher requires Adapter.
// ReloadInterval periodically reloads PolicyFile or an external Adapter.
// LoadPolicy remains available for synchronous programmatic reloads.
type Config struct {
	Model             string                  `yaml:"model"`
	ModelFile         string                  `yaml:"model_file"`
	Policy            string                  `yaml:"policy"`
	PolicyFile        string                  `yaml:"policy_file"`
	Adapter           ProviderRef             `yaml:"adapter"`
	Watcher           ProviderRef             `yaml:"watcher"`
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

// Validate checks source selection, provider references, and request-policy
// settings without doing filesystem I/O. File readability and model/policy
// syntax are checked when the enforcer is built during construction.
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
	adapter           normalizedProviderRef
	watcher           normalizedProviderRef
	requestConvention RequestConvention
	missingPermission MissingPermissionPolicy
	reloadInterval    time.Duration
}

type normalizedProviderRef struct {
	key      plugin.Key
	instance string
}

func (r normalizedProviderRef) enabled() bool { return r.key != "" }

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

	adapter, err := normalizeProviderRef("adapter", c.Adapter)
	if err != nil {
		return normalizedConfig{}, err
	}
	watcher, err := normalizeProviderRef("watcher", c.Watcher)
	if err != nil {
		return normalizedConfig{}, err
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
	if adapter.enabled() && (policySet || policyFile != "") {
		return normalizedConfig{}, fmt.Errorf("casbin: adapter is mutually exclusive with policy and policy_file")
	}
	if watcher.enabled() && !adapter.enabled() {
		return normalizedConfig{}, fmt.Errorf("casbin: watcher requires adapter")
	}
	if c.ReloadInterval < 0 {
		return normalizedConfig{}, fmt.Errorf("casbin: reload_interval cannot be negative")
	}
	if c.ReloadInterval > 0 && policyFile == "" && !adapter.enabled() {
		return normalizedConfig{}, fmt.Errorf("casbin: reload_interval requires policy_file or adapter")
	}

	return normalizedConfig{
		model:             c.Model,
		modelFile:         modelFile,
		policy:            c.Policy,
		policyFile:        policyFile,
		adapter:           adapter,
		watcher:           watcher,
		requestConvention: convention,
		missingPermission: missing,
		reloadInterval:    c.ReloadInterval,
	}, nil
}

func normalizeProviderRef(field string, ref ProviderRef) (normalizedProviderRef, error) {
	if strings.TrimSpace(ref.Plugin) != ref.Plugin {
		return normalizedProviderRef{}, fmt.Errorf("casbin: %s.plugin must not have surrounding whitespace", field)
	}
	if strings.TrimSpace(ref.Instance) != ref.Instance {
		return normalizedProviderRef{}, fmt.Errorf("casbin: %s.instance must not have surrounding whitespace", field)
	}
	instance := plugin.NormalizeInstance(ref.Instance)
	if err := plugin.ValidateInstanceName(instance); err != nil {
		return normalizedProviderRef{}, fmt.Errorf("casbin: %s.instance: %w", field, err)
	}
	if ref.Plugin == "" {
		if instance != plugin.DefaultInstance {
			return normalizedProviderRef{}, fmt.Errorf("casbin: %s.instance %q requires %s.plugin", field, instance, field)
		}
		return normalizedProviderRef{}, nil
	}
	if err := plugin.ValidateName(ref.Plugin); err != nil {
		return normalizedProviderRef{}, fmt.Errorf("casbin: %s.plugin: %w", field, err)
	}
	return normalizedProviderRef{key: plugin.Key(ref.Plugin), instance: instance}, nil
}
