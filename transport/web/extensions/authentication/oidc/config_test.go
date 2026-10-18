package oidc

import "testing"

func TestDefaultConfigMountsUnderTheAdapterOwnPrefix(t *testing.T) {
	config := DefaultConfig()
	if config.Prefix != "/auth/oidc" {
		t.Fatalf("default prefix = %q, want /auth/oidc", config.Prefix)
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
	if normalized.prefix != "/auth/oidc" {
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
		"relative":           "auth/oidc",
		"empty segment":      "/auth//oidc",
		"dot segment":        "/auth/./oidc",
		"dot-dot segment":    "/auth/../oidc",
		"path parameter":     "/auth/:tenant/oidc",
		"wildcard":           "/auth/*",
		"brace parameter":    "/auth/{tenant}/oidc",
		"query delimiter":    "/auth/oidc?x=1",
		"fragment delimiter": "/auth/oidc#x",
		"backslash":          `/auth\oidc`,
		"escape":             "/auth/%2e%2e/oidc",
		"whitespace":         "/auth oidc",
		"control character":  "/auth/oidc\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			if err := (Config{Prefix: prefix}).Validate(); err == nil {
				t.Fatalf("prefix %q was accepted", prefix)
			}
		})
	}
}

func TestConfigAcceptsLiteralNestedPrefixes(t *testing.T) {
	for _, prefix := range []string{"/auth/oidc", "/api/v1/auth/oidc", "/oidc-v2", "/a_b/c.d"} {
		if err := (Config{Prefix: prefix}).Validate(); err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
	}
}
