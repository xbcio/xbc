package swagger

import (
	"fmt"
	"path"
	"strings"
)

// Config is bound from plugins.swagger.
type Config struct {
	Title       string   `yaml:"title"       default:"XBC API"`
	Version     string   `yaml:"version"     default:"dev"`
	Description string   `yaml:"description"`
	JSONPath    string   `yaml:"json_path"   default:"/openapi.json"`
	UIPath      string   `yaml:"ui_path"     default:"/docs"`
	UIEnabled   bool     `yaml:"ui_enabled"  default:"true"`
	Servers     []string `yaml:"servers"`
	BearerAuth  bool     `yaml:"bearer_auth" default:"true"`
}

type settings struct {
	title       string
	version     string
	description string
	jsonPath    string
	uiPath      string
	uiEnabled   bool
	servers     []string
	bearerAuth  bool
}

// DefaultConfig returns Swagger's production-safe configuration defaults.
func DefaultConfig() Config {
	return Config{
		Title:      "XBC API",
		Version:    "dev",
		JSONPath:   "/openapi.json",
		UIPath:     "/docs",
		UIEnabled:  true,
		BearerAuth: true,
	}
}

func prepareConfig(cfg Config) (Config, error) {
	if _, err := normalizeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeConfig(cfg Config) (settings, error) {
	title := strings.TrimSpace(cfg.Title)
	if title == "" {
		title = "XBC API"
	}
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = "dev"
	}

	jsonPath, err := normalizeEndpoint("json_path", cfg.JSONPath, "/openapi.json")
	if err != nil {
		return settings{}, err
	}
	uiPath, err := normalizeEndpoint("ui_path", cfg.UIPath, "/docs")
	if err != nil {
		return settings{}, err
	}
	if cfg.UIEnabled {
		if jsonPath == uiPath || strings.HasPrefix(jsonPath, uiPath+"/") {
			return settings{}, fmt.Errorf("swagger: json_path %q must not overlap ui_path %q", jsonPath, uiPath)
		}
	}

	servers := make([]string, 0, len(cfg.Servers))
	seen := make(map[string]struct{}, len(cfg.Servers))
	for _, server := range cfg.Servers {
		server = strings.TrimSpace(server)
		if server == "" {
			return settings{}, fmt.Errorf("swagger: server URL cannot be empty")
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}

	return settings{
		title:       title,
		version:     version,
		description: cfg.Description,
		jsonPath:    jsonPath,
		uiPath:      uiPath,
		uiEnabled:   cfg.UIEnabled,
		servers:     servers,
		bearerAuth:  cfg.BearerAuth,
	}, nil
}

func normalizeEndpoint(field, value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("swagger: %s must be an absolute path", field)
	}
	if strings.ContainsAny(value, ":*") {
		return "", fmt.Errorf("swagger: %s must be a static path", field)
	}
	clean := path.Clean(value)
	if clean == "/" {
		return "", fmt.Errorf("swagger: %s cannot be the root path", field)
	}
	return strings.TrimSuffix(clean, "/"), nil
}
