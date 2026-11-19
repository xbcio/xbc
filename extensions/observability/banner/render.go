package banner

import (
	"strings"
	"unicode"

	"github.com/common-nighthawk/go-figure"
)

// renderBanner returns the single message the plugin logs. It never returns
// ANSI escapes: the art renderer is used without its color variant, and text
// from the runtime is sanitized here because only configured text passed
// through validation.
func renderBanner(cfg normalizedConfig, text string) string {
	text = normalizeText(text)
	if cfg.style == StylePlain || text == "" {
		return text
	}
	// strict is false on purpose. The library's strict mode calls the standard
	// library's log.Fatal on a rune it cannot render, which would terminate
	// the process from a display concern; non-strict substitutes '?' instead.
	return strings.TrimRight(figure.NewFigure(text, cfg.font, false).String(), "\n")
}

// normalizeText collapses every run of whitespace into one space and replaces
// control characters, so a banner is always printable single-line input and
// can never smuggle a terminal escape sequence into the output.
func normalizeText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}
