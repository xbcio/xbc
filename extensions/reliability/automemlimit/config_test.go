package automemlimit

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestDefaultConfigPinsTheDocumentedDefaults freezes the values
// plugins.automemlimit starts from. The ratio deliberately matches the
// automemlimit library's own default instead of inventing a second one.
func TestDefaultConfigPinsTheDocumentedDefaults(t *testing.T) {
	want := Config{
		Ratio:           0.9,
		MinBytes:        0,
		Fallback:        "none",
		RefreshInterval: 0,
	}
	if got := DefaultConfig(); got != want {
		t.Fatalf("DefaultConfig() = %#v, want %#v", got, want)
	}
}

// TestValidateRejectsInvalidValues checks that every rejected field names the
// configuration key an operator has to edit.
func TestValidateRejectsInvalidValues(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*Config)
		wantKey string
	}{
		"zero ratio": {
			mutate:  func(cfg *Config) { cfg.Ratio = 0 },
			wantKey: "plugins.automemlimit.ratio",
		},
		"negative ratio": {
			mutate:  func(cfg *Config) { cfg.Ratio = -0.5 },
			wantKey: "plugins.automemlimit.ratio",
		},
		"ratio above one": {
			mutate:  func(cfg *Config) { cfg.Ratio = 1.5 },
			wantKey: "plugins.automemlimit.ratio",
		},
		"not a number": {
			mutate:  func(cfg *Config) { cfg.Ratio = math.NaN() },
			wantKey: "plugins.automemlimit.ratio",
		},
		"positive infinity": {
			mutate:  func(cfg *Config) { cfg.Ratio = math.Inf(1) },
			wantKey: "plugins.automemlimit.ratio",
		},
		"negative minimum": {
			mutate:  func(cfg *Config) { cfg.MinBytes = -1 },
			wantKey: "plugins.automemlimit.min_bytes",
		},
		"unknown fallback": {
			mutate:  func(cfg *Config) { cfg.Fallback = "container" },
			wantKey: "plugins.automemlimit.fallback",
		},
		"negative refresh interval": {
			mutate:  func(cfg *Config) { cfg.RefreshInterval = -time.Second },
			wantKey: "plugins.automemlimit.refresh_interval",
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			testCase.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate(%#v) succeeded", cfg)
			}
			if !strings.Contains(err.Error(), testCase.wantKey) {
				t.Fatalf("Validate() error = %q, want it to name %s", err, testCase.wantKey)
			}
			if _, prepareErr := prepareConfig(cfg); prepareErr == nil {
				t.Fatalf("prepareConfig(%#v) succeeded", cfg)
			}
		})
	}
}

// TestValidateAcceptsBoundaryAndNormalizedValues pins the accepted spelling of
// each field: the ratio's inclusive upper bound, an omitted fallback (empty
// means "none"), and the documented zero refresh interval.
func TestValidateAcceptsBoundaryAndNormalizedValues(t *testing.T) {
	for name, cfg := range map[string]Config{
		"defaults": DefaultConfig(),
		"ratio at the upper bound": {
			Ratio:    1,
			Fallback: "system",
		},
		"fallback omitted": {
			Ratio: 0.5,
		},
		"fallback none explicit": {
			Ratio:    0.5,
			Fallback: "none",
		},
		"positive refresh interval": {
			Ratio:           0.5,
			Fallback:        "none",
			RefreshInterval: 30 * time.Second,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%#v) = %v", cfg, err)
			}
		})
	}

	normalized, err := normalizeConfig(Config{Ratio: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.fallback != fallbackNone {
		t.Fatalf("normalized fallback = %q, want %q for an omitted value", normalized.fallback, fallbackNone)
	}
}
