package csrf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// currentRouteKeyForTest is the request-scoped key web.CurrentRoute reads. The
// Web package keeps the constant private, so an extension test that wants to
// drive the matched-route branch publishes the value under the same key the
// production middleware does.
const currentRouteKeyForTest = "xbc/web.currentRoute"

const testSecret = "0123456789abcdef0123456789abcdef"

var allMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodHead,
	http.MethodOptions,
	http.MethodTrace,
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

// newEngineForPolicy registers every method under each path with a chain that
// optionally publishes the frozen route, then runs the middleware and the
// counted downstream handler.
func newEngineForPolicy(t *testing.T, p *Plugin, route *web.RouteInfo, downstream *int, paths ...string) *enginetest.Engine {
	t.Helper()
	if len(paths) == 0 {
		paths = []string{"/resource"}
	}
	engine := enginetest.New()
	for _, path := range paths {
		chain := make([]web.Handler, 0, 3)
		if route != nil {
			info := *route
			chain = append(chain, func(_ context.Context, c *web.Ctx) error {
				c.Set(currentRouteKeyForTest, info)
				c.Next()
				return nil
			})
		}
		chain = append(chain, p.Handler())
		chain = append(chain, func(_ context.Context, c *web.Ctx) error {
			*downstream++
			c.Status(http.StatusOK)
			return nil
		})
		for _, method := range allMethods {
			engine.Handle(method, path, chain)
		}
	}
	return engine
}

func newTestEngine(t *testing.T, cfg Config, route *web.RouteInfo, downstream *int, paths ...string) *enginetest.Engine {
	t.Helper()
	return newEngineForPolicy(t, newTestPlugin(t, cfg), route, downstream, paths...)
}

func perform(engine http.Handler, method, target string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	for _, apply := range mutate {
		apply(request)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func withHeader(name, value string) func(*http.Request) {
	return func(request *http.Request) { request.Header.Add(name, value) }
}

func withCookie(name, value string) func(*http.Request) {
	return func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

func requirePassed(t *testing.T, response *httptest.ResponseRecorder, downstream int) {
	t.Helper()
	if response.Code != http.StatusOK || downstream != 1 {
		t.Fatalf("status/downstream = %d/%d, want 200/1", response.Code, downstream)
	}
}

func requireForbidden(t *testing.T, response *httptest.ResponseRecorder, downstream int) {
	t.Helper()
	if response.Code != http.StatusForbidden || downstream != 0 {
		t.Fatalf("status/downstream = %d/%d, want 403/0", response.Code, downstream)
	}
	var problem map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("refusal body is not JSON: %v (%q)", err, response.Body.String())
	}
	if problem["code"] != "forbidden" {
		t.Fatalf("refusal problem code = %v, want forbidden", problem["code"])
	}
}

func allowlistedConfig() Config {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://app.example.com"}
	return cfg
}

func tokenConfig() Config {
	cfg := DefaultConfig()
	cfg.Mode = "token"
	cfg.Secret = testSecret
	return cfg
}

func ctxWithCookie(name, value string) *web.Ctx {
	return ctxWithCookies(name, value)
}

func ctxWithCookies(name string, values ...string) *web.Ctx {
	request := httptest.NewRequest(http.MethodPost, "http://example.com/resource", nil)
	for _, value := range values {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	return enginetest.NewCtx(httptest.NewRecorder(), request)
}

func TestOriginMode(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		method   string
		target   string
		mutate   []func(*http.Request)
		wantPass bool
	}{
		{
			name:     "same-origin POST passes",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com")},
			wantPass: true,
		},
		{
			name:     "same-origin HTTPS POST passes",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "https://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "https://example.com")},
			wantPass: true,
		},
		{
			name:     "cross-origin POST is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "https://evil.example")},
			wantPass: false,
		},
		{
			name:     "allow-listed origin passes",
			cfg:      allowlistedConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "https://app.example.com")},
			wantPass: true,
		},
		{
			name:     "allow-listed origin with a trailing slash passes",
			cfg:      allowlistedConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "https://app.example.com/")},
			wantPass: true,
		},
		{
			name:     "same-origin Referer passes without Origin",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Referer", "http://example.com/login")},
			wantPass: true,
		},
		{
			name:     "allow-listed Referer origin passes without Origin",
			cfg:      allowlistedConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Referer", "https://app.example.com/form")},
			wantPass: true,
		},
		{
			name:     "absent Origin and Referer is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			wantPass: false,
		},
		{
			name:     "two Origin values are refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com"), withHeader("Origin", "https://evil.example")},
			wantPass: false,
		},
		{
			name:     "control character in Origin is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com\x01")},
			wantPass: false,
		},
		{
			name:     "control character in Referer is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Referer", "http://example.com/\x7f")},
			wantPass: false,
		},
		{
			name:     "unparsable Referer is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Referer", "http://[::1")},
			wantPass: false,
		},
		{
			name:     "relative Referer is refused",
			cfg:      DefaultConfig(),
			method:   http.MethodPost,
			target:   "http://example.com/resource",
			mutate:   []func(*http.Request){withHeader("Referer", "/login")},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downstream := 0
			engine := newTestEngine(t, tt.cfg, nil, &downstream)
			response := perform(engine, tt.method, tt.target, tt.mutate...)
			if tt.wantPass {
				requirePassed(t, response, downstream)
				return
			}
			requireForbidden(t, response, downstream)
		})
	}
}

func TestSafeMethodsAlwaysPassInOriginMode(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			downstream := 0
			engine := newTestEngine(t, DefaultConfig(), nil, &downstream)
			response := perform(engine, method, "http://example.com/resource")
			requirePassed(t, response, downstream)
		})
	}
}

func TestSafeMethodsAlwaysPassInTokenMode(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			downstream := 0
			engine := newTestEngine(t, tokenConfig(), nil, &downstream)
			response := perform(engine, method, "http://example.com/resource")
			requirePassed(t, response, downstream)
		})
	}
}

func TestOriginModeSecFetchSite(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		mutate   []func(*http.Request)
		wantPass bool
	}{
		{
			name:     "cross-site with a same-origin Origin is refused",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com"), withHeader("Sec-Fetch-Site", "cross-site")},
			wantPass: false,
		},
		{
			name:     "cross-site with an allow-listed origin passes",
			cfg:      allowlistedConfig(),
			mutate:   []func(*http.Request){withHeader("Origin", "https://app.example.com"), withHeader("Sec-Fetch-Site", "cross-site")},
			wantPass: true,
		},
		{
			name:     "same-site with a same-origin Origin is refused",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com"), withHeader("Sec-Fetch-Site", "same-site")},
			wantPass: false,
		},
		{
			name:     "none defers to a same-origin Referer",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Referer", "http://example.com/page"), withHeader("Sec-Fetch-Site", "none")},
			wantPass: true,
		},
		{
			name:     "none with a cross-origin Referer is refused",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Referer", "https://evil.example/page"), withHeader("Sec-Fetch-Site", "none")},
			wantPass: false,
		},
		{
			name:     "same-origin with a cross-origin Origin is refused",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Origin", "https://evil.example"), withHeader("Sec-Fetch-Site", "same-origin")},
			wantPass: false,
		},
		{
			name:     "unknown value is refused",
			cfg:      DefaultConfig(),
			mutate:   []func(*http.Request){withHeader("Origin", "http://example.com"), withHeader("Sec-Fetch-Site", "same-origin-ish")},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downstream := 0
			engine := newTestEngine(t, tt.cfg, nil, &downstream)
			response := perform(engine, http.MethodPost, "http://example.com/resource", tt.mutate...)
			if tt.wantPass {
				requirePassed(t, response, downstream)
				return
			}
			requireForbidden(t, response, downstream)
		})
	}
}

func TestTokenIsStableAndSessionBound(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)

	first := p.Token(ctxWithCookie(cfg.SessionCookie, "session-a"))
	second := p.Token(ctxWithCookie(cfg.SessionCookie, "session-a"))
	other := p.Token(ctxWithCookie(cfg.SessionCookie, "session-b"))
	if first == "" {
		t.Fatal("Token() returned an empty string for a present session cookie")
	}
	if first != second {
		t.Fatalf("Token() is not stable for one session: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("Token() is identical for different sessions: %q", first)
	}
}

func TestTokenReturnsEmptyWithoutAUsableCookie(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)

	if got := p.Token(enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/resource", nil))); got != "" {
		t.Fatalf("Token() without a cookie = %q, want empty", got)
	}
	if got := p.Token(ctxWithCookie(cfg.SessionCookie, "")); got != "" {
		t.Fatalf("Token() with an empty cookie value = %q, want empty", got)
	}
	if got := p.Token(ctxWithCookie(cfg.SessionCookie, strings.Repeat("a", maxSessionCookieBytes+1))); got != "" {
		t.Fatalf("Token() with an oversized cookie = %q, want empty", got)
	}
}

func TestTokenForMatchesTokenAndReturnsEmptyOutsideTokenMode(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)

	forSession := p.TokenFor("session-a")
	if forSession == "" {
		t.Fatal("TokenFor() returned an empty token for a session")
	}
	if got := p.Token(ctxWithCookie(cfg.SessionCookie, "session-a")); got != forSession {
		t.Fatalf("Token() = %q, TokenFor() = %q; want the same token", got, forSession)
	}
	if got := p.TokenFor("session-b"); got == "" || got == forSession {
		t.Fatalf("TokenFor(session-b) = %q, want a distinct non-empty token", got)
	}
	if got := p.TokenFor(""); got != "" {
		t.Fatalf("TokenFor(\"\") = %q, want empty", got)
	}
	if got := p.TokenFor(strings.Repeat("a", maxSessionCookieBytes+1)); got != "" {
		t.Fatalf("TokenFor(oversized) = %q, want empty", got)
	}

	origin := newTestPlugin(t, DefaultConfig())
	if got := origin.Token(ctxWithCookie(defaultSessionCookie, "session-a")); got != "" {
		t.Fatalf("Token() in origin mode = %q, want empty", got)
	}
	if got := origin.TokenFor("session-a"); got != "" {
		t.Fatalf("TokenFor() in origin mode = %q, want empty", got)
	}

	// A token-mode policy without a secret cannot exist through New, but both
	// entry points must stay harmless if one is ever constructed directly.
	withoutSecret := &Plugin{policy: &policy{mode: modeToken}}
	if got := withoutSecret.Token(ctxWithCookie(defaultSessionCookie, "session-a")); got != "" {
		t.Fatalf("Token() without a secret = %q, want empty", got)
	}
	if got := withoutSecret.TokenFor("session-a"); got != "" {
		t.Fatalf("TokenFor() without a secret = %q, want empty", got)
	}
}

// TestTokenForBootstrapsAFreshLoginResponse reproduces the login flow: the
// session plugin's SetCookie writes the new cookie to the response and never
// mutates the request, so only TokenFor with the installed session can return
// a token on that response. The token must then verify an unsafe request that
// carries the cookie.
func TestTokenForBootstrapsAFreshLoginResponse(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)

	request := httptest.NewRequest(http.MethodPost, "http://example.com/login", nil)
	c := enginetest.NewCtx(httptest.NewRecorder(), request)
	if got := p.Token(c); got != "" {
		t.Fatalf("Token() before login = %q, want empty", got)
	}
	http.SetCookie(c.Writer(), &http.Cookie{Name: cfg.SessionCookie, Value: "login-session"})
	if got := p.Token(c); got != "" {
		t.Fatalf("Token() after SetCookie = %q, want empty: SetCookie writes the response, not the request", got)
	}

	token := p.TokenFor("login-session")
	if token == "" {
		t.Fatal("TokenFor() returned an empty token for the installed session")
	}

	downstream := 0
	engine := newEngineForPolicy(t, p, nil, &downstream)
	response := perform(engine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, "login-session"), withHeader(cfg.Header, token))
	requirePassed(t, response, downstream)
}

func TestDuplicateSessionCookiesAreRefused(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)
	token := p.TokenFor("session-a")
	if token == "" {
		t.Fatal("TokenFor() returned an empty token for a session")
	}

	downstream := 0
	engine := newEngineForPolicy(t, p, nil, &downstream)
	response := perform(engine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, "session-a"),
		withCookie(cfg.SessionCookie, "session-b"),
		withHeader(cfg.Header, token))
	requireForbidden(t, response, downstream)

	if got := p.Token(ctxWithCookies(cfg.SessionCookie, "session-a", "session-b")); got != "" {
		t.Fatalf("Token() with duplicate session cookies = %q, want empty", got)
	}
}

func TestTokenMode(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)
	matching := p.Token(ctxWithCookie(cfg.SessionCookie, "session-a"))
	otherSession := p.Token(ctxWithCookie(cfg.SessionCookie, "session-b"))
	if matching == "" || otherSession == "" {
		t.Fatal("Token() returned an empty value for a present session cookie")
	}

	tests := []struct {
		name     string
		mutate   []func(*http.Request)
		wantPass bool
	}{
		{
			name:     "matching header passes",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, matching)},
			wantPass: true,
		},
		{
			name:     "wrong token of equal length is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, otherSession)},
			wantPass: false,
		},
		{
			name:     "wrong token of different length is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, "AAAA")},
			wantPass: false,
		},
		{
			name:     "missing header is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a")},
			wantPass: false,
		},
		{
			name:     "empty header is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, "")},
			wantPass: false,
		},
		{
			name:     "missing session cookie is refused",
			mutate:   []func(*http.Request){withHeader(cfg.Header, matching)},
			wantPass: false,
		},
		{
			name:     "oversized session cookie is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, strings.Repeat("a", maxSessionCookieBytes+1)), withHeader(cfg.Header, matching)},
			wantPass: false,
		},
		{
			name:     "invalid base64 token is refused",
			mutate:   []func(*http.Request){withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, "!!!!")},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downstream := 0
			engine := newEngineForPolicy(t, p, nil, &downstream)
			response := perform(engine, http.MethodPost, "http://example.com/resource", tt.mutate...)
			if tt.wantPass {
				requirePassed(t, response, downstream)
				return
			}
			requireForbidden(t, response, downstream)
		})
	}
}

func TestTokenModeAcceptsExactlyMaxCookieLength(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)
	session := strings.Repeat("a", maxSessionCookieBytes)
	token := p.Token(ctxWithCookie(cfg.SessionCookie, session))
	if token == "" {
		t.Fatal("Token() returned an empty string for a cookie at the size limit")
	}

	downstream := 0
	engine := newEngineForPolicy(t, p, nil, &downstream)
	response := perform(engine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, session), withHeader(cfg.Header, token))
	requirePassed(t, response, downstream)
}

func TestTokenRotationInvalidatesOldToken(t *testing.T) {
	cfg := tokenConfig()
	p := newTestPlugin(t, cfg)
	oldToken := p.Token(ctxWithCookie(cfg.SessionCookie, "old-session"))
	newToken := p.Token(ctxWithCookie(cfg.SessionCookie, "new-session"))
	if oldToken == "" || newToken == "" || oldToken == newToken {
		t.Fatalf("tokens = %q/%q, want two distinct non-empty values", oldToken, newToken)
	}

	oldDownstream, newDownstream := 0, 0
	oldEngine := newEngineForPolicy(t, p, nil, &oldDownstream)
	oldResponse := perform(oldEngine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, "new-session"), withHeader(cfg.Header, oldToken))
	requireForbidden(t, oldResponse, oldDownstream)

	newEngine := newEngineForPolicy(t, p, nil, &newDownstream)
	newResponse := perform(newEngine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, "new-session"), withHeader(cfg.Header, newToken))
	requirePassed(t, newResponse, newDownstream)
}

func TestTokenModeHonorsConfiguredNames(t *testing.T) {
	cfg := tokenConfig()
	cfg.Header = "X-Custom-CSRF"
	cfg.SessionCookie = "my_session"
	p := newTestPlugin(t, cfg)

	token := p.Token(ctxWithCookie(cfg.SessionCookie, "session-a"))
	if token == "" {
		t.Fatal("Token() returned an empty string for the configured cookie name")
	}

	downstream := 0
	engine := newEngineForPolicy(t, p, nil, &downstream)
	response := perform(engine, http.MethodPost, "http://example.com/resource",
		withCookie(cfg.SessionCookie, "session-a"), withHeader(cfg.Header, token))
	requirePassed(t, response, downstream)

	downstream = 0
	engine = newEngineForPolicy(t, p, nil, &downstream)
	response = perform(engine, http.MethodPost, "http://example.com/resource",
		withCookie("xbc_session", "session-a"), withHeader(cfg.Header, token))
	requireForbidden(t, response, downstream)
}

func TestExemptionsApplyPerRouteInBothModes(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodPost, Path: "/items/:id", Name: "items.update"}
	modes := []struct {
		name string
		cfg  Config
	}{
		{"origin", DefaultConfig()},
		{"token", tokenConfig()},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			tests := []struct {
				name     string
				edit     func(*Config)
				wantPass bool
			}{
				{"excluded route name", func(c *Config) { c.ExcludeRoutes = []string{"items.update"} }, true},
				{"excluded route template", func(c *Config) { c.ExcludePaths = []string{"/items/:id"} }, true},
				{"excluded raw path", func(c *Config) { c.ExcludePaths = []string{"/items/42"} }, true},
				{"non-matching exclusion is refused", func(c *Config) { c.ExcludePaths = []string{"/other/*"} }, false},
				{"non-matching route name is refused", func(c *Config) { c.ExcludeRoutes = []string{"items.show"} }, false},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					cfg := mode.cfg
					tt.edit(&cfg)
					downstream := 0
					engine := newTestEngine(t, cfg, &route, &downstream, "/items/{id}")
					// No Origin, cookie, or token is sent: only the exemption can
					// admit these state-changing requests.
					response := perform(engine, http.MethodPost, "http://example.com/items/42")
					if tt.wantPass {
						requirePassed(t, response, downstream)
						return
					}
					requireForbidden(t, response, downstream)
				})
			}
		})
	}
}

func TestExcludedRawPathAppliesWithoutAFrozenRoute(t *testing.T) {
	t.Run("excluded", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ExcludePaths = []string{"/webhooks/*"}
		downstream := 0
		engine := newTestEngine(t, cfg, nil, &downstream, "/webhooks/github")
		response := perform(engine, http.MethodPost, "http://example.com/webhooks/github")
		requirePassed(t, response, downstream)
	})
	t.Run("not excluded", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ExcludePaths = []string{"/other/*"}
		downstream := 0
		engine := newTestEngine(t, cfg, nil, &downstream, "/webhooks/github")
		response := perform(engine, http.MethodPost, "http://example.com/webhooks/github")
		requireForbidden(t, response, downstream)
	})
}
