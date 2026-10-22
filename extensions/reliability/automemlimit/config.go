package automemlimit

import (
	"fmt"
	"math"
	"time"
)

const (
	// defaultRatio matches the automemlimit library's own default: 90% of the
	// container limit.
	defaultRatio = 0.9

	fallbackNone   = "none"
	fallbackSystem = "system"
)

// Config is bound from plugins.automemlimit.
type Config struct {
	// Ratio is the fraction of the cgroup limit installed as GOMEMLIMIT. It
	// must be in (0,1]; the AUTOMEMLIMIT environment variable can override it
	// at install time.
	Ratio float64 `yaml:"ratio" default:"0.9"`
	// MinBytes raises the installed limit to at least this many bytes when it
	// is positive. Zero installs the ratio result unchanged.
	MinBytes int64 `yaml:"min_bytes" default:"0"`
	// Fallback selects the provider behavior when the cgroup limit cannot be
	// read: "none" fails through to the plugin's error handling, while "system"
	// falls back to the host's physical memory.
	Fallback string `yaml:"fallback" default:"none"`
	// RefreshInterval re-reads and re-applies the limit on this interval. Zero
	// installs once during Init and submits no managed task.
	RefreshInterval time.Duration `yaml:"refresh_interval" default:"0s"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		Ratio:    defaultRatio,
		Fallback: fallbackNone,
	}
}

// Validate checks the ratio, the minimum, the fallback mode, and the refresh
// interval. Every error names the configuration key it rejects.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// normalizedConfig is the validated configuration the Plugin runs with.
type normalizedConfig struct {
	ratio           float64
	minBytes        int64
	fallback        string
	refreshInterval time.Duration
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	if math.IsNaN(cfg.Ratio) || math.IsInf(cfg.Ratio, 0) || cfg.Ratio <= 0 || cfg.Ratio > 1 {
		return normalizedConfig{}, fmt.Errorf(
			"automemlimit: plugins.automemlimit.ratio must be in (0,1], got %v", cfg.Ratio)
	}
	if cfg.MinBytes < 0 {
		return normalizedConfig{}, fmt.Errorf(
			"automemlimit: plugins.automemlimit.min_bytes must not be negative, got %d", cfg.MinBytes)
	}
	fallback := cfg.Fallback
	if fallback == "" {
		fallback = fallbackNone
	}
	if fallback != fallbackNone && fallback != fallbackSystem {
		return normalizedConfig{}, fmt.Errorf(
			"automemlimit: plugins.automemlimit.fallback must be %q or %q, got %q", fallbackNone, fallbackSystem, cfg.Fallback)
	}
	if cfg.RefreshInterval < 0 {
		return normalizedConfig{}, fmt.Errorf(
			"automemlimit: plugins.automemlimit.refresh_interval must not be negative, got %s", cfg.RefreshInterval)
	}
	return normalizedConfig{
		ratio:           cfg.Ratio,
		minBytes:        cfg.MinBytes,
		fallback:        fallback,
		refreshInterval: cfg.RefreshInterval,
	}, nil
}

// prepareConfig is the pure ConfigSpec preparation step: doctor and the
// snapshot tooling can run it without constructing the plugin or touching the
// process.
func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
