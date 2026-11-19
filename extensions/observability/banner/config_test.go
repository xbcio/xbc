package banner

import (
	"strings"
	"testing"
)

// TestDefaultConfigPinsTheDocumentedDefaults freezes the values
// plugins.banner starts from. Empty text is deliberate: the runtime identity
// is only known once a Context exists.
func TestDefaultConfigPinsTheDocumentedDefaults(t *testing.T) {
	want := Config{
		Text:  "",
		Font:  "standard",
		Style: StyleASCII,
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
		"unknown style": {
			mutate:  func(cfg *Config) { cfg.Style = "art" },
			wantKey: "plugins.banner.style",
		},
		"unknown font": {
			mutate:  func(cfg *Config) { cfg.Font = "no-such-font" },
			wantKey: "plugins.banner.font",
		},
		"text with a line break": {
			mutate:  func(cfg *Config) { cfg.Text = "first\nsecond" },
			wantKey: "plugins.banner.text",
		},
		"text with a tab": {
			mutate:  func(cfg *Config) { cfg.Text = "first\tsecond" },
			wantKey: "plugins.banner.text",
		},
		"text with an escape sequence": {
			mutate:  func(cfg *Config) { cfg.Text = "\x1b[31mred" },
			wantKey: "plugins.banner.text",
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
// each field: an omitted style and font mean the documented defaults, both
// styles are accepted, a bundled non-default font is accepted, and text may
// carry printable punctuation.
func TestValidateAcceptsBoundaryAndNormalizedValues(t *testing.T) {
	for name, cfg := range map[string]Config{
		"defaults": DefaultConfig(),
		"omitted style and font": {
			Text: "order service",
		},
		"plain style": {
			Text:  "order service",
			Style: StylePlain,
		},
		"explicit ascii and font": {
			Text:  "order service",
			Style: StyleASCII,
			Font:  "banner",
		},
		"printable punctuation": {
			Text: "order-service/v2 (blue)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%#v) = %v", cfg, err)
			}
		})
	}
}

// TestNormalizeConfigAppliesDefaults pins the normalized form the factory and
// the render path consume.
func TestNormalizeConfigAppliesDefaults(t *testing.T) {
	normalized, err := normalizeConfig(Config{Text: "  orders  "})
	if err != nil {
		t.Fatal(err)
	}
	want := normalizedConfig{text: "orders", font: "standard", style: StyleASCII}
	if normalized != want {
		t.Fatalf("normalizeConfig() = %#v, want %#v", normalized, want)
	}

	normalized, err = normalizeConfig(Config{Style: StylePlain})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.style != StylePlain {
		t.Fatalf("normalized style = %q, want %q", normalized.style, StylePlain)
	}
}

// TestPrepareConfigIsPureAndValidating pins the ConfigSpec contract: the
// preparation step validates without constructing anything.
func TestPrepareConfigIsPureAndValidating(t *testing.T) {
	cfg := DefaultConfig()
	prepared, err := prepareConfig(cfg)
	if err != nil {
		t.Fatalf("prepareConfig(%#v) = %v", cfg, err)
	}
	if prepared != cfg {
		t.Fatalf("prepareConfig() = %#v, want the input unchanged", prepared)
	}

	if _, err := prepareConfig(Config{Style: "unknown"}); err == nil {
		t.Fatal("prepareConfig accepted an unknown style")
	}
}

// TestValidFontsContainsTheDefault keeps the embedded-font probe honest: an
// empty list or a missing default would make every explicit font fail.
func TestValidFontsContainsTheDefault(t *testing.T) {
	if len(validFonts) == 0 {
		t.Fatal("validFonts is empty; the embedded font set was not discovered")
	}
	if _, ok := validFonts[defaultFont]; !ok {
		t.Fatalf("validFonts does not contain the default font %q", defaultFont)
	}
}
