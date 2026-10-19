package saml

import "testing"

func TestDefaultConfigUsesTheSAMLRoutePrefix(t *testing.T) {
	config := DefaultConfig()
	if config.Prefix != "/auth/saml" {
		t.Fatalf("default prefix = %q, want /auth/saml", config.Prefix)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v", err)
	}
}

func TestNormalizeConfigTrimsThePrefix(t *testing.T) {
	normalized, err := normalizeConfig(Config{Prefix: "  /api/v1/auth/saml/  "})
	if err != nil {
		t.Fatalf("normalizeConfig: %v", err)
	}
	if normalized.prefix != "/api/v1/auth/saml" {
		t.Fatalf("prefix = %q", normalized.prefix)
	}

	normalized, err = normalizeConfig(Config{})
	if err != nil || normalized.prefix != defaultPrefix {
		t.Fatalf("empty config normalized to %q, %v", normalized.prefix, err)
	}
}

func TestNormalizeConfigRejectsUnsafePrefixes(t *testing.T) {
	for name, prefix := range map[string]string{
		"relative":             "auth/saml",
		"root":                 "/",
		"route parameter":      "/auth/:tenant",
		"wildcard":             "/auth/*",
		"brace parameter":      "/auth/{tenant}",
		"dot segment":          "/auth/../saml",
		"empty segment":        "/auth//saml",
		"query delimiter":      "/auth/saml?x=1",
		"fragment delimiter":   "/auth/saml#x",
		"backslash":            `/auth\saml`,
		"percent escape":       "/auth/%73aml",
		"control character":    "/auth/sa\nml",
		"trailing dot segment": "/auth/saml/.",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeConfig(Config{Prefix: prefix}); err == nil {
				t.Fatalf("normalizeConfig(%q) = nil error", prefix)
			}
		})
	}
}
