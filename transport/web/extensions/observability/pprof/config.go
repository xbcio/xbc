package pprof

import (
	"fmt"
	"path"
	"strings"
)

const defaultPath = "/debug/pprof"

// Config is bound from plugins.pprof.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path" default:"/debug/pprof"`
}

type settings struct {
	enabled bool
	path    string
}

func defaultConfig() Config {
	return Config{Path: defaultPath}
}

func normalizeConfig(cfg Config) (settings, error) {
	endpointPath := strings.TrimSpace(cfg.Path)
	if endpointPath == "" {
		endpointPath = defaultPath
	}
	if !strings.HasPrefix(endpointPath, "/") || strings.ContainsAny(endpointPath, ":*?#") {
		return settings{}, fmt.Errorf("pprof: path must be a static absolute HTTP path")
	}
	endpointPath = strings.TrimSuffix(path.Clean(endpointPath), "/")
	if endpointPath == "" || endpointPath == "/" {
		return settings{}, fmt.Errorf("pprof: path cannot be the root path")
	}

	return settings{
		enabled: cfg.Enabled,
		path:    endpointPath,
	}, nil
}
