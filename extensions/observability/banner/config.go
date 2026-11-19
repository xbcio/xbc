package banner

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/common-nighthawk/go-figure"
)

const (
	// StyleASCII renders the banner as multi-line ASCII art.
	StyleASCII = "ascii"
	// StylePlain renders one plain line, for an environment that does not
	// want the oversized art.
	StylePlain = "plain"

	defaultFont = "standard"
	defaultText = "XBC"
)

// Config is bound from plugins.banner.
type Config struct {
	// Text is the banner text. Empty selects the runtime service name, then
	// the process instance, then the fixed fallback "XBC".
	Text string `yaml:"text"`
	// Font names one of the bundled ASCII-art fonts. Empty selects
	// "standard"; the plain style ignores the value but a configured one is
	// still validated.
	Font string `yaml:"font" default:"standard"`
	// Style selects the art ("ascii") or one plain line ("plain"). Empty
	// selects "ascii".
	Style string `yaml:"style" default:"ascii"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{Font: defaultFont, Style: StyleASCII}
}

// validFonts is the set of font names the pinned library embeds. Asking the
// library for the names keeps the check honest: rendering an unknown font
// panics inside the library rather than returning an error, so validation has
// to happen before the first render.
var validFonts = func() map[string]struct{} {
	names := make(map[string]struct{})
	for _, asset := range figure.AssetNames() {
		name, ok := strings.CutPrefix(asset, "fonts/")
		if !ok {
			continue
		}
		if name, ok = strings.CutSuffix(name, ".flf"); ok {
			names[name] = struct{}{}
		}
	}
	return names
}()

// Validate checks the style, the font, and the text. Every error names the
// configuration key it rejects.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// normalizedConfig is the validated configuration the Plugin runs with.
type normalizedConfig struct {
	text  string
	font  string
	style string
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	style := strings.TrimSpace(cfg.Style)
	if style == "" {
		style = StyleASCII
	}
	if style != StyleASCII && style != StylePlain {
		return normalizedConfig{}, fmt.Errorf(
			"banner: plugins.banner.style must be %q or %q, got %q", StyleASCII, StylePlain, cfg.Style)
	}

	font := strings.TrimSpace(cfg.Font)
	if font == "" {
		font = defaultFont
	}
	if _, ok := validFonts[font]; !ok {
		return normalizedConfig{}, fmt.Errorf(
			"banner: plugins.banner.font names unknown font %q; use one of the fonts bundled with the renderer, such as %q", cfg.Font, defaultFont)
	}

	if strings.IndexFunc(cfg.Text, unicode.IsControl) >= 0 {
		return normalizedConfig{}, fmt.Errorf(
			"banner: plugins.banner.text must not contain control characters or line breaks")
	}

	return normalizedConfig{
		text:  strings.TrimSpace(cfg.Text),
		font:  font,
		style: style,
	}, nil
}

// prepareConfig is the pure ConfigSpec preparation step: doctor and the
// snapshot tooling can run it without constructing the plugin or rendering
// anything.
func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
