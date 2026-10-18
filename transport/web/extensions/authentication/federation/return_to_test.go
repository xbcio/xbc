package federation

import (
	"strings"
	"testing"
)

func testReturnToPolicy(t *testing.T) returnToPolicy {
	t.Helper()
	policy, err := newReturnToPolicy("/home", []string{"https://app.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestReturnToSanitizeAcceptsSameOriginRelativePaths(t *testing.T) {
	policy := testReturnToPolicy(t)
	for _, raw := range []string{
		"/",
		"/dashboard",
		"/a/b?tab=1&sort=desc",
		"/a/b#section",
		"/~user/%E4%B8%AD%E6%96%87",
		"/path%20with%20spaces",
	} {
		if got := policy.sanitize(raw); got != raw {
			t.Errorf("sanitize(%q) = %q, want it accepted unchanged", raw, got)
		}
	}
}

func TestReturnToSanitizeRejectsHostileValues(t *testing.T) {
	policy := testReturnToPolicy(t)
	for name, raw := range map[string]string{
		"empty":                     "",
		"relative without a slash":  "dashboard",
		"leading space":             " /dashboard",
		"scheme relative":           "//evil.example/dashboard",
		"backslash scheme relative": `/\evil.example`,
		"backslash in path":         `/a\b`,
		"absolute other origin":     "https://evil.example/dashboard",
		"lookalike suffix":          "https://app.example.com.evil.example/",
		"plain http":                "http://app.example.com/dashboard",
		"javascript scheme":         "javascript:alert(document.cookie)",
		"data scheme":               "data:text/html,<script>alert(1)</script>",
		"userinfo":                  "https://user@app.example.com/dashboard",
		"userinfo disguise":         "https://app.example.com@evil.example/dashboard",
		"encoded slash":             "/%2f%2fevil.example",
		"encoded backslash":         "/%5Cevil.example",
		"encoded CRLF":              "/ok%0d%0aLocation:%20https://evil.example",
		"encoded NUL":               "/ok%00",
		"raw CR":                    "/ok\r",
		"raw LF":                    "/ok\n",
		"raw NUL":                   "/ok\x00",
		"raw tab":                   "/ok\t",
		"overlong relative path":    "/" + strings.Repeat("a", MaxReturnToLength),
		"overlong absolute URL":     "https://app.example.com/" + strings.Repeat("a", MaxReturnToLength),
		"empty host":                "https:///dashboard",
		"fragment only":             "#fragment",
		"query only":                "?tab=1",
	} {
		t.Run(name, func(t *testing.T) {
			if got := policy.sanitize(raw); got != "/home" {
				t.Fatalf("sanitize(%q) = %q, want the configured fallback", raw, got)
			}
		})
	}
}

func TestReturnToSanitizeAcceptsAllowlistedAbsoluteURLs(t *testing.T) {
	policy := testReturnToPolicy(t)
	for name, raw := range map[string]string{
		"exact origin":        "https://app.example.com/dashboard",
		"uppercase host":      "https://APP.example.com/dashboard",
		"uppercase scheme":    "HTTPS://app.example.com/dashboard",
		"default port":        "https://app.example.com:443/dashboard",
		"query and fragment":  "https://app.example.com/a?b=c#d",
		"origin without path": "https://app.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			if got := policy.sanitize(raw); got != raw {
				t.Fatalf("sanitize(%q) = %q, want it accepted unchanged", raw, got)
			}
		})
	}
}

func TestReturnToSanitizeRejectsUnlistedPorts(t *testing.T) {
	policy := testReturnToPolicy(t)
	if got := policy.sanitize("https://app.example.com:8443/dashboard"); got != "/home" {
		t.Fatalf("sanitize() = %q, want the fallback for an unlisted port", got)
	}
}

func TestReturnToPolicyRequiresAReachableFallback(t *testing.T) {
	for name, fallback := range map[string]string{
		"scheme relative":      "//evil.example",
		"absolute unlisted":    "https://evil.example/",
		"plain http":           "http://app.example.com/",
		"relative without one": "home",
		"backslash":            `/home\..\evil`,
		"empty":                "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newReturnToPolicy(fallback, []string{"https://app.example.com"}); err == nil {
				t.Fatalf("newReturnToPolicy(%q) = nil error", fallback)
			}
		})
	}
}

func TestReturnToPolicyAcceptsAnAllowlistedAbsoluteFallback(t *testing.T) {
	policy, err := newReturnToPolicy("https://app.example.com/after-login", []string{"https://app.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.fallback != "https://app.example.com/after-login" {
		t.Fatalf("fallback = %q", policy.fallback)
	}
	if got := policy.sanitize("https://evil.example/"); got != policy.fallback {
		t.Fatalf("sanitize() = %q, want the configured fallback", got)
	}
}

func TestReturnToPolicyRejectsInvalidOrigins(t *testing.T) {
	for name, origin := range map[string]string{
		"plain http":      "http://app.example.com",
		"relative path":   "/app",
		"no host":         "https://",
		"path":            "https://app.example.com/login",
		"query":           "https://app.example.com?x=1",
		"fragment":        "https://app.example.com#x",
		"userinfo":        "https://user@app.example.com",
		"backslash":       `https://app.example.com\@evil.example`,
		"control":         "https://app.example.com\n",
		"empty":           "",
		"scheme-relative": "//app.example.com",
		"other scheme":    "wss://app.example.com",
		"overlong origin": "https://app.example.com/" + strings.Repeat("a", MaxReturnToLength),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newReturnToPolicy("/", []string{origin}); err == nil {
				t.Fatalf("newReturnToPolicy accepted origin %q", origin)
			}
		})
	}
}

func TestCanonicalOriginNormalizesHostAndPort(t *testing.T) {
	for name, testCase := range map[string]struct {
		raw  string
		want string
	}{
		"lowercase host": {"https://APP.example.com", "https://app.example.com"},
		"default port":   {"https://app.example.com:443", "https://app.example.com"},
		"trailing slash": {"https://app.example.com:443/", "https://app.example.com"},
		"explicit port":  {"https://app.example.com:8443", "https://app.example.com:8443"},
		"IPv6 port":      {"https://[::1]:8443", "https://[::1]:8443"},
		"IPv6 default":   {"https://[::1]", "https://[::1]"},
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := newReturnToPolicy("/", []string{testCase.raw})
			if err != nil {
				t.Fatalf("newReturnToPolicy(%q) error = %v", testCase.raw, err)
			}
			if len(policy.origins) != 1 || !policy.origins[testCase.want] {
				t.Fatalf("origins = %v, want %q", policy.origins, testCase.want)
			}
		})
	}
}
