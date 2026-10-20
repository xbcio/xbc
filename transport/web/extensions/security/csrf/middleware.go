package csrf

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

// maxSessionCookieBytes bounds the cookie value before it is hashed. The value
// must be the session identifier, not an arbitrarily large carrier, and the
// bound keeps a hostile cookie from turning every refused request into a large
// HMAC input.
const maxSessionCookieBytes = 4096

type mode uint8

const (
	modeOrigin mode = iota
	modeToken
)

// policy is the immutable, request-independent result of compiling a Config.
// A Plugin holds one instance for its whole lifetime and never mutates it, so
// concurrent requests read it without synchronization.
type policy struct {
	mode          mode
	allowOrigins  map[string]struct{}
	excludePaths  []string
	excludeRoutes map[string]struct{}
	header        string
	sessionCookie string
	secret        []byte
}

func compilePolicy(cfg Config) (*policy, error) {
	requestMode, err := parseMode(cfg.Mode)
	if err != nil {
		return nil, err
	}
	origins, err := normalizeAllowOrigins(cfg.AllowOrigins)
	if err != nil {
		return nil, err
	}
	if !isToken(cfg.Header) {
		return nil, &ConfigError{Field: "header", Message: "must be a valid HTTP header name"}
	}
	if !isToken(cfg.SessionCookie) {
		return nil, &ConfigError{Field: "session_cookie", Message: "must be a valid cookie name"}
	}
	if err := validateExcludePaths(cfg.ExcludePaths); err != nil {
		return nil, err
	}
	excludeRoutes, err := normalizeExcludeRoutes(cfg.ExcludeRoutes)
	if err != nil {
		return nil, err
	}
	secret := []byte(cfg.Secret)
	if len(secret) != 0 && len(secret) < minSecretBytes {
		return nil, &ConfigError{Field: "secret", Message: "must be at least 32 bytes"}
	}
	if requestMode == modeToken && len(secret) == 0 {
		return nil, &ConfigError{Field: "secret", Message: "is required in token mode"}
	}

	return &policy{
		mode:          requestMode,
		allowOrigins:  origins,
		excludePaths:  append([]string(nil), cfg.ExcludePaths...),
		excludeRoutes: excludeRoutes,
		header:        cfg.Header,
		sessionCookie: cfg.SessionCookie,
		secret:        secret,
	}, nil
}

// ConfigError identifies an invalid semantic configuration field.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string { return "csrf: " + e.Field + " " + e.Message }

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	if p == nil || p.policy == nil {
		web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
		return nil
	}
	p.policy.handle(c)
	return nil
}

func (p *policy) handle(c *web.Ctx) {
	request := c.Request()
	if request == nil || safeMethod(request.Method) {
		c.Next()
		return
	}
	if p.exempt(c) {
		c.Next()
		return
	}
	allowed := false
	if p.mode == modeToken {
		allowed = p.tokenAllowed(c)
	} else {
		allowed = p.originAllowed(c)
	}
	if !allowed {
		web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
		return
	}
	c.Next()
}

// safeMethod reports whether method is defined as safe by RFC 9110: it must not
// change state, so it is not a CSRF target. HEAD is listed explicitly instead
// of being inferred from GET.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// exempt reports whether the current route is declared outside CSRF
// protection. The raw request path is checked even when no frozen route is
// available, so unmatched requests can still be exempted; the route template is
// checked in addition so one declaration covers every concrete path it matches.
// Exemption is always per route: there is deliberately no global switch.
func (p *policy) exempt(c *web.Ctx) bool {
	var rawPath string
	if request := c.Request(); request != nil && request.URL != nil {
		rawPath = request.URL.Path
	}
	route, matched := web.CurrentRoute(c)
	if !matched {
		return matchAnyPath(p.excludePaths, rawPath)
	}
	if _, excluded := p.excludeRoutes[route.Name]; excluded {
		return true
	}
	return matchAnyPath(p.excludePaths, route.Path) || matchAnyPath(p.excludePaths, rawPath)
}

func matchAnyPath(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if matched, err := path.Match(pattern, value); err == nil && matched {
			return true
		}
	}
	return false
}

// originAllowed implements origin mode. It accepts the request only when its
// origin is the request's own origin or exactly one configured allowlist
// entry, and only when a present Sec-Fetch-Site header agrees with that
// decision.
func (p *policy) originAllowed(c *web.Ctx) bool {
	request := c.Request()
	if request == nil {
		return false
	}

	origins := request.Header.Values("Origin")
	var origin string
	switch len(origins) {
	case 0:
		referer := request.Header.Get("Referer")
		if referer == "" {
			// Neither Origin nor Referer: a browser always sends at least one
			// for a state-changing request. A non-browser client is admitted by
			// an explicit route exemption, never by a default gap.
			return false
		}
		if containsControl(referer) {
			return false
		}
		parsed, err := url.Parse(referer)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return false
		}
		origin = strings.ToLower(parsed.Scheme) + "://" + parsed.Host
	case 1:
		origin = origins[0]
		if containsControl(origin) {
			return false
		}
	default:
		// More than one Origin value is ambiguous; browsers send exactly one.
		return false
	}

	origin = strings.TrimSuffix(origin, "/")
	sameOrigin := p.isSameOrigin(origin, request)
	_, allowedOrigin := p.allowOrigins[origin]

	return p.fetchSiteAgrees(c.GetHeader("Sec-Fetch-Site"), sameOrigin, allowedOrigin)
}

// isSameOrigin compares origin against the request's own scheme and Host
// without building the request origin string, so the Origin-header path stays
// allocation-free apart from reading the header itself. The request's TLS
// state decides the scheme; a reverse proxy that terminates TLS must be
// configured to preserve the origin the browser saw.
func (p *policy) isSameOrigin(origin string, request *http.Request) bool {
	prefix := "http://"
	if request.TLS != nil {
		prefix = "https://"
	}
	return len(origin) >= len(prefix) && origin[:len(prefix)] == prefix && origin[len(prefix):] == request.Host
}

// fetchSiteAgrees applies the Sec-Fetch-Site constraint when a browser sends
// it. "same-origin" and "none" defer to the origin decision; "same-site" and
// "cross-site" require the origin to have matched the explicit allowlist, so a
// cross-site request cannot ride on the same-origin branch even if a proxy
// rewrote Origin. Any other value is refused.
func (p *policy) fetchSiteAgrees(fetchSite string, sameOrigin, allowedOrigin bool) bool {
	switch fetchSite {
	case "":
		return sameOrigin || allowedOrigin
	case "same-origin", "none":
		return sameOrigin || allowedOrigin
	case "same-site", "cross-site":
		return allowedOrigin
	default:
		return false
	}
}

// sessionID returns the session cookie value carried by request. Exactly one
// cookie with the configured name must be present -- the same rule the session
// extractor applies -- because a duplicated cookie is ambiguous rather than a
// silent first-wins, and the value is bounded before it becomes HMAC input.
func (p *policy) sessionID(request *http.Request) (string, bool) {
	cookies := request.CookiesNamed(p.sessionCookie)
	if len(cookies) != 1 || cookies[0] == nil || cookies[0].Value == "" || len(cookies[0].Value) > maxSessionCookieBytes {
		return "", false
	}
	return cookies[0].Value, true
}

// tokenAllowed implements token mode: the session cookie must be present and
// bounded, the configured header must carry a base64url HMAC of that cookie's
// raw value, and the two are compared in constant time.
func (p *policy) tokenAllowed(c *web.Ctx) bool {
	request := c.Request()
	if request == nil {
		return false
	}
	session, ok := p.sessionID(request)
	if !ok {
		return false
	}
	presented := request.Header.Get(p.header)
	if presented == "" {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(presented)
	if err != nil {
		return false
	}
	return hmac.Equal(decoded, p.tokenFor(session))
}

// tokenFor derives the raw HMAC-SHA256 of the session identifier. Binding the
// secret to the session is what makes rotating the session invalidate every
// token issued for the old one without any server-side token store.
func (p *policy) tokenFor(session string) []byte {
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(session))
	return mac.Sum(nil)
}

func (p *policy) tokenString(session string) string {
	return base64.RawURLEncoding.EncodeToString(p.tokenFor(session))
}
