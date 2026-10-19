package oauth2

import "testing"

func TestDefaultConfigMountsUnderTheAdapterOwnPrefix(t *testing.T) {
	config := DefaultConfig()
	if config.Prefix != "/auth/oauth2" {
		t.Fatalf("default prefix = %q, want /auth/oauth2", config.Prefix)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v", err)
	}
}

func TestNormalizeConfigDefaultsAndTrimsTrailingSlashes(t *testing.T) {
	normalized, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatalf("normalizeConfig(empty) error = %v", err)
	}
	if normalized.prefix != "/auth/oauth2" {
		t.Fatalf("empty prefix normalized to %q", normalized.prefix)
	}

	normalized, err = normalizeConfig(Config{Prefix: "  /auth/sso//  "})
	if err != nil {
		t.Fatalf("normalizeConfig(trailing slashes) error = %v", err)
	}
	if normalized.prefix != "/auth/sso" {
		t.Fatalf("prefix normalized to %q, want /auth/sso", normalized.prefix)
	}
}

func TestConfigRejectsUnsafePrefixes(t *testing.T) {
	for name, prefix := range map[string]string{
		"root":               "/",
		"relative":           "auth/oauth2",
		"empty segment":      "/auth//oauth2",
		"dot segment":        "/auth/./oauth2",
		"dot-dot segment":    "/auth/../oauth2",
		"path parameter":     "/auth/:tenant/oauth2",
		"wildcard":           "/auth/*",
		"brace parameter":    "/auth/{tenant}/oauth2",
		"query delimiter":    "/auth/oauth2?x=1",
		"fragment delimiter": "/auth/oauth2#x",
		"backslash":          `/auth\oauth2`,
		"escape":             "/auth/%2e%2e/oauth2",
		"whitespace":         "/auth oauth2",
		"control character":  "/auth/oauth2\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			if err := (Config{Prefix: prefix}).Validate(); err == nil {
				t.Fatalf("prefix %q was accepted", prefix)
			}
		})
	}
}

func TestConfigAcceptsLiteralNestedPrefixes(t *testing.T) {
	for _, prefix := range []string{"/auth/oauth2", "/api/v1/auth/oauth2", "/oauth2-v2", "/a_b/c.d"} {
		if err := (Config{Prefix: prefix}).Validate(); err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
	}
}
