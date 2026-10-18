package federation

import (
	"fmt"
	"net/url"
	"strings"
)

// MaxReturnToLength is the longest return_to the policy considers. A longer
// value is discarded like any other unacceptable one.
const MaxReturnToLength = 2048

// returnToPolicy decides where a login may land. The default is a same-origin
// relative path; an absolute URL is accepted only when its origin appears in
// the explicitly configured allowlist.
type returnToPolicy struct {
	// fallback is a validated same-origin relative path, never empty.
	fallback string
	// origins is the canonical set of allowed absolute origins, each written
	// as https://host[:port] with a lowercase host and no default port.
	origins map[string]bool
}

// newReturnToPolicy builds a policy from the configured default landing and
// absolute-origin allowlist. The default landing itself may be an absolute URL
// only when its origin is allowlisted; a configuration that names an
// unreachable landing is rejected instead of silently falling back.
func newReturnToPolicy(fallback string, allowedOrigins []string) (returnToPolicy, error) {
	origins := make(map[string]bool, len(allowedOrigins))
	for _, raw := range allowedOrigins {
		origin, err := parseAllowedOrigin(raw)
		if err != nil {
			return returnToPolicy{}, fmt.Errorf("federation-http: allowed_return_origins: %w", err)
		}
		if origins[origin] {
			return returnToPolicy{}, fmt.Errorf("federation-http: allowed_return_origins contains %q twice", origin)
		}
		origins[origin] = true
	}

	policy := returnToPolicy{origins: origins}
	if path, ok := sanitizeRelativePath(fallback); ok {
		policy.fallback = path
		return policy, nil
	}
	if _, ok := policy.absoluteAllowed(fallback); ok {
		policy.fallback = fallback
		return policy, nil
	}
	return returnToPolicy{}, fmt.Errorf("federation-http: default_return_to must be a same-origin relative path or an allowlisted https origin")
}

// sanitize returns raw when the policy accepts it and the configured fallback
// otherwise. An unacceptable value is never reported as an error: the browser
// learns nothing about why its return_to was replaced.
func (p returnToPolicy) sanitize(raw string) string {
	if raw == "" || len(raw) > MaxReturnToLength || containsControl(raw) || strings.ContainsRune(raw, '\\') {
		return p.fallback
	}
	if strings.HasPrefix(raw, "/") {
		if path, ok := sanitizeRelativePath(raw); ok {
			return path
		}
		return p.fallback
	}
	if target, ok := p.absoluteAllowed(raw); ok {
		return target
	}
	return p.fallback
}

// absoluteAllowed parses raw as an absolute URL and reports whether its origin
// is allowlisted and its shape is safe to send in a Location header.
func (p returnToPolicy) absoluteAllowed(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxReturnToLength || containsControl(raw) || strings.ContainsRune(raw, '\\') {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", false
	}
	if !p.origins[canonicalOrigin(parsed)] {
		return "", false
	}
	return raw, true
}

// sanitizeRelativePath accepts a same-origin absolute path: exactly one
// leading slash, no backslash, no control characters, and no percent-encoded
// delimiter that a downstream decoder could turn into one.
func sanitizeRelativePath(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxReturnToLength || raw[0] != '/' || containsControl(raw) {
		return "", false
	}
	if strings.HasPrefix(raw, "//") || strings.ContainsRune(raw, '\\') {
		return "", false
	}
	lower := strings.ToLower(raw)
	for _, encoded := range []string{"%2f", "%5c", "%00", "%0d", "%0a"} {
		if strings.Contains(lower, encoded) {
			return "", false
		}
	}
	return raw, true
}

// parseAllowedOrigin validates one allowlist entry and returns its canonical
// form: scheme https, lowercase host, and no default port.
func parseAllowedOrigin(raw string) (string, error) {
	if raw == "" || len(raw) > MaxReturnToLength || containsControl(raw) || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("origin %q contains control characters or a backslash", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("origin %q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("origin %q must use https", raw)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("origin %q has no host", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("origin %q must not carry user information", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("origin %q must not carry a path", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("origin %q must not carry a query or fragment", raw)
	}
	return canonicalOrigin(parsed), nil
}

// canonicalOrigin renders the scheme and authority of parsed with a lowercase
// host and without the default https port.
func canonicalOrigin(parsed *url.URL) string {
	hostname := strings.ToLower(parsed.Hostname())
	if strings.ContainsRune(hostname, ':') {
		hostname = "[" + hostname + "]"
	}
	port := parsed.Port()
	if port == "" || port == "443" {
		return "https://" + hostname
	}
	return "https://" + hostname + ":" + port
}
