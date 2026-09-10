package gracefulshutdown

import (
	"fmt"
	"path"
	"strings"
)

const defaultPath = "/-/shutdown"

// Config is bound from plugins.gracefulshutdown. The HTTP endpoint is disabled
// by default.
type Config struct {
	HTTP HTTPConfig `yaml:"http"`
}

// HTTPConfig controls the optional operator endpoint.
type HTTPConfig struct {
	Enabled bool   `yaml:"enabled" default:"false"`
	Path    string `yaml:"path" default:"/-/shutdown"`
}

type endpointConfig struct {
	enabled bool
	path    string
}

func defaultConfig() Config {
	return Config{HTTP: HTTPConfig{
		Path: defaultPath,
	}}
}

func normalizeConfig(cfg Config) (endpointConfig, error) {
	endpointPath := strings.TrimSpace(cfg.HTTP.Path)
	if endpointPath == "" {
		endpointPath = defaultPath
	}
	if !strings.HasPrefix(endpointPath, "/") || strings.ContainsAny(endpointPath, ":*") {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.path must be a static absolute path")
	}
	endpointPath = path.Clean(endpointPath)
	if endpointPath == "/" {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.path cannot be the root path")
	}

	return endpointConfig{
		enabled: cfg.HTTP.Enabled,
		path:    endpointPath,
	}, nil
}
