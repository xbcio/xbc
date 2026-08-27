// config/source.go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Options carries every knob Load needs to locate and assemble the
// configuration tree.
type Options struct {
	File      string         // --config; when non-empty the file MUST exist
	Profile   string         // --profile or XBC_PROFILE
	EnvPrefix string         // "XBC_"; threaded through to later Bind calls by the caller, unused here
	Overrides map[string]any // flat dotted-path -> value, e.g. from --set flags
}

// loadKoanf searches for the config file, merges the profile overlay,
// applies Overrides, and returns the koanf instance. Missing files are only
// an error when Options.File named one explicitly (ruling R8).
func loadKoanf(opts Options) (*koanf.Koanf, error) {
	k := koanf.New(".")

	basePath, err := locateBaseFile(opts.File)
	if err != nil {
		return nil, err
	}

	if basePath != "" {
		if err := k.Load(file.Provider(basePath), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("xbc: 读取配置文件 %s 失败：%w", basePath, err)
		}

		if opts.Profile != "" {
			profilePath := profileSibling(basePath, opts.Profile)
			if _, statErr := os.Stat(profilePath); statErr == nil {
				if err := k.Load(file.Provider(profilePath), yaml.Parser()); err != nil {
					return nil, fmt.Errorf("xbc: 读取 profile 配置文件 %s 失败：%w", profilePath, err)
				}
			}
			// A missing profile file is silently skipped: it is an optional
			// overlay, not an explicit promise like --config.
		}
	}

	if len(opts.Overrides) > 0 {
		if err := k.Load(confmap.Provider(opts.Overrides, "."), nil); err != nil {
			return nil, fmt.Errorf("xbc: 应用配置覆盖失败：%w", err)
		}
	}

	return k, nil
}

// profileSibling derives "application-prod.yml" from "application.yml" + "prod".
func profileSibling(basePath, profile string) string {
	dir := filepath.Dir(basePath)
	ext := filepath.Ext(basePath)
	name := filepath.Base(basePath)
	name = name[:len(name)-len(ext)]
	return filepath.Join(dir, name+"-"+profile+ext)
}

// locateBaseFile resolves the main config file path, or "" when none of the
// three lookup locations has one and that is legal (ruling R8).
func locateBaseFile(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("xbc: 指定的配置文件 %s 不存在", explicitPath)
			}
			return "", fmt.Errorf("xbc: 无法访问配置文件 %s：%w", explicitPath, err)
		}
		return explicitPath, nil
	}

	for _, candidate := range []string{"application.yml", filepath.Join("configs", "application.yml")} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", nil
}
