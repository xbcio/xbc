package swag

import (
	"fmt"
	"path"
	"strings"
)

// Config is bound from plugins.swag.
type Config struct {
	InstanceName string `yaml:"instance_name" default:"swagger"`
	JSONPath     string `yaml:"json_path"     default:"/swagger.json"`
	UIPath       string `yaml:"ui_path"       default:"/docs"`
	UIEnabled    bool   `yaml:"ui_enabled"    default:"true"`
}

type settings struct {
	instanceName string
	jsonPath     string
	uiPath       string
	uiEnabled    bool
}

// DefaultConfig returns Swag's production-safe configuration defaults.
func DefaultConfig() Config {
	return Config{
		InstanceName: "swagger",
		JSONPath:     "/swagger.json",
		UIPath:       "/docs",
		UIEnabled:    true,
	}
}

func prepareConfig(cfg Config) (Config, error) {
	if _, err := normalizeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeConfig(cfg Config) (settings, error) {
	instanceName := strings.TrimSpace(cfg.InstanceName)
	if instanceName == "" {
		return settings{}, fmt.Errorf("swag: instance_name cannot be empty")
	}

	jsonPath, err := normalizeEndpoint("json_path", cfg.JSONPath, "/swagger.json")
	if err != nil {
		return settings{}, err
	}
	uiPath, err := normalizeEndpoint("ui_path", cfg.UIPath, "/docs")
	if err != nil {
		return settings{}, err
	}
	if cfg.UIEnabled && (jsonPath == uiPath || strings.HasPrefix(jsonPath, uiPath+"/")) {
		return settings{}, fmt.Errorf("swag: json_path %q must not overlap ui_path %q", jsonPath, uiPath)
	}

	return settings{
		instanceName: instanceName,
		jsonPath:     jsonPath,
		uiPath:       uiPath,
		uiEnabled:    cfg.UIEnabled,
	}, nil
}

func normalizeEndpoint(field, value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("swag: %s must be an absolute path", field)
	}
	if strings.ContainsAny(value, ":*") {
		return "", fmt.Errorf("swag: %s must be a static path", field)
	}
	clean := path.Clean(value)
	if clean == "/" {
		return "", fmt.Errorf("swag: %s cannot be the root path", field)
	}
	return strings.TrimSuffix(clean, "/"), nil
}
