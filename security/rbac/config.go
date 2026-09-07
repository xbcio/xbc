package rbac

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

const (
	defaultBackendPlugin = "casbin"
	defaultAdminRole     = "admin"
)

// ProviderRef selects one exact Backend exporter. Empty fields select
// casbin's default instance.
type ProviderRef struct {
	Plugin   string `yaml:"plugin"   default:"casbin"`
	Instance string `yaml:"instance" default:"default"`
}

// Config is bound from plugins.rbac.
type Config struct {
	Backend   ProviderRef `yaml:"backend"`
	AdminRole string      `yaml:"admin_role" default:"admin"`
}

// DefaultConfig returns the default casbin[default] backend and administrator
// role used by both Definition-driven and direct construction.
func DefaultConfig() Config {
	return Config{
		Backend: ProviderRef{
			Plugin:   defaultBackendPlugin,
			Instance: plugin.DefaultInstance,
		},
		AdminRole: defaultAdminRole,
	}
}

// Validate checks the exact backend reference and administrator role without
// resolving or calling the backend.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type normalizedConfig struct {
	backend   plugin.Identity
	adminRole string
}

func prepareConfig(c Config) (Config, error) {
	normalized, err := normalizeConfig(c)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Backend: ProviderRef{
			Plugin:   normalized.backend.Plugin.String(),
			Instance: normalized.backend.Instance,
		},
		AdminRole: normalized.adminRole,
	}, nil
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	backendPlugin := c.Backend.Plugin
	if strings.TrimSpace(backendPlugin) != backendPlugin {
		return normalizedConfig{}, fmt.Errorf("rbac: backend.plugin must not have surrounding whitespace")
	}
	if backendPlugin == "" {
		backendPlugin = defaultBackendPlugin
	}
	if err := plugin.ValidateName(backendPlugin); err != nil {
		return normalizedConfig{}, fmt.Errorf("rbac: backend.plugin: %w", err)
	}

	backendInstance := c.Backend.Instance
	if strings.TrimSpace(backendInstance) != backendInstance {
		return normalizedConfig{}, fmt.Errorf("rbac: backend.instance must not have surrounding whitespace")
	}
	backendInstance = plugin.NormalizeInstance(backendInstance)
	if err := plugin.ValidateInstanceName(backendInstance); err != nil {
		return normalizedConfig{}, fmt.Errorf("rbac: backend.instance: %w", err)
	}

	adminRole := c.AdminRole
	if adminRole == "" {
		adminRole = defaultAdminRole
	} else {
		adminRole = strings.TrimSpace(adminRole)
		if adminRole == "" {
			return normalizedConfig{}, fmt.Errorf("rbac: admin_role must not be empty")
		}
	}

	return normalizedConfig{
		backend: plugin.Identity{
			Plugin:   plugin.Key(backendPlugin),
			Instance: backendInstance,
		},
		adminRole: adminRole,
	}, nil
}
