package banner

import (
	"strings"
	"testing"
)

// TestRenderASCIIProducesTrimmedArtWithoutANSI pins the shape the log record
// receives: the art without its trailing newline, and no escape sequences.
func TestRenderASCIIProducesTrimmedArtWithoutANSI(t *testing.T) {
	got := renderBanner(normalizedConfig{style: StyleASCII, font: "standard"}, "XBC")
	if strings.HasSuffix(got, "\n") {
		t.Fatal("renderBanner kept the library's trailing newline")
	}
	if lines := strings.Split(got, "\n"); len(lines) != 5 {
		t.Fatalf("renderBanner produced %d lines, want the standard font's 5", len(lines))
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("renderBanner produced an ANSI escape sequence")
	}
	if !strings.Contains(got, "____") {
		t.Fatalf("renderBanner = %q, want recognizable art for XBC", got)
	}
}

// TestRenderPlainNormalizesText covers text that reaches the renderer from the
// runtime rather than from validated configuration.
func TestRenderPlainNormalizesText(t *testing.T) {
	got := renderBanner(normalizedConfig{style: StylePlain}, "  orders\t\napi\x1b ")
	if got != "orders api" {
		t.Fatalf("renderBanner() = %q, want %q", got, "orders api")
	}
}

// TestRenderASCIIAndPlainAgreeOnEmptyText pins the degenerate case: an empty
// text renders as an empty message rather than a font-shaped block of spaces.
func TestRenderASCIIAndPlainAgreeOnEmptyText(t *testing.T) {
	for _, style := range []string{StyleASCII, StylePlain} {
		if got := renderBanner(normalizedConfig{style: style, font: "standard"}, "   "); got != "" {
			t.Fatalf("renderBanner(%s, empty) = %q, want the empty string", style, got)
		}
	}
}
