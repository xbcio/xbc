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
	EnvPrefix string         // "XBC_"; names the environment layer and later Bind calls
	Overrides map[string]any // flat dotted-path -> value, e.g. from --set flags
	// Universe declares who owns which configuration path. When it is nil no
	// environment layer is merged and no ownership check runs, which is what
	// an embedding caller that assembles its own tree wants. The runtime
	// always supplies one.
	Universe *Universe
}

// layer records which paths one configuration source contributed, in merge
// order. It is the whole provenance story: values never leave this file, so a
// diagnostic built from a layer can name a path but never print a secret.
type layer struct {
	label string
	paths []string
}

// loadKoanf searches for the config file, merges the profile overlay,
// Overrides and finally the environment layer, and returns the koanf instance
// together with the ordered provenance of every path. Missing files are only
// an error when Options.File named one explicitly (ruling R8).
func loadKoanf(opts Options) (*koanf.Koanf, []layer, error) {
	if opts.EnvPrefix == "" {
		// An empty prefix would make the environment layer claim every variable
		// in the process, so it is defaulted here as well as in Load.
		opts.EnvPrefix = DefaultEnvPrefix
	}
	k := koanf.New(".")
	var layers []layer

	merge := func(label string, source *koanf.Koanf) error {
		paths := source.Keys()
		if len(paths) == 0 {
			return nil
		}
		if err := k.Merge(source); err != nil {
			return fmt.Errorf("xbc: failed to merge configuration source %s: %w", label, err)
		}
		layers = append(layers, layer{label: label, paths: paths})
		return nil
	}

	basePath, err := locateBaseFile(opts.File)
	if err != nil {
		return nil, nil, err
	}

	if basePath != "" {
		base := koanf.New(".")
		if err := base.Load(file.Provider(basePath), yaml.Parser()); err != nil {
			return nil, nil, fmt.Errorf("xbc: failed to read configuration file %s: %w", basePath, err)
		}
		if err := merge("file "+basePath, base); err != nil {
			return nil, nil, err
		}

		if opts.Profile != "" {
			profilePath := profileSibling(basePath, opts.Profile)
			if _, statErr := os.Stat(profilePath); statErr == nil {
				overlay := koanf.New(".")
				if err := overlay.Load(file.Provider(profilePath), yaml.Parser()); err != nil {
					return nil, nil, fmt.Errorf("xbc: failed to read profile configuration file %s: %w", profilePath, err)
				}
				if err := merge("profile "+profilePath, overlay); err != nil {
					return nil, nil, err
				}
			}
			// A missing profile file is silently skipped: it is an optional
			// overlay, not an explicit promise like --config.
		}
	}

	if len(opts.Overrides) > 0 {
		overrides := koanf.New(".")
		if err := overrides.Load(confmap.Provider(opts.Overrides, "."), nil); err != nil {
			return nil, nil, fmt.Errorf("xbc: failed to apply configuration override: %w", err)
		}
		if err := merge("override", overrides); err != nil {
			return nil, nil, err
		}
	}

	// The environment layer comes last so that a container-injected variable
	// wins over every file, and so that Exists sees an ENV-only section --
	// which is what lets a WhenConfigured plugin activate from ENV alone.
	if opts.Universe != nil {
		values, err := opts.Universe.envOverlay(opts.EnvPrefix, os.Environ(), k)
		if err != nil {
			return nil, nil, err
		}
		if len(values) > 0 {
			environment := koanf.New(".")
			if err := environment.Load(confmap.Provider(values, "."), nil); err != nil {
				return nil, nil, fmt.Errorf("xbc: failed to apply environment configuration: %w", err)
			}
			if err := merge("env", environment); err != nil {
				return nil, nil, err
			}
		}
		if err := opts.Universe.checkOwnership(k); err != nil {
			return nil, nil, err
		}
	}

	return k, layers, nil
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
				return "", fmt.Errorf("xbc: specified configuration file %s does not exist", explicitPath)
			}
			return "", fmt.Errorf("xbc: cannot access configuration file %s: %w", explicitPath, err)
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
