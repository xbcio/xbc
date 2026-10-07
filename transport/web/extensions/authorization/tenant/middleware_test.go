package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

// authenticationExemptKeyForTest mirrors the private request-scoped key the
// authentication middleware sets when it resolves a request to permit (see
// web.AuthenticationExempt). Tests that exercise tenant standalone, without
// the real authentication middleware in front of it, set this key directly
// to simulate an exempt request -- mirroring the existing currentRouteKeyForTest
// convention above.
const authenticationExemptKeyForTest = "xbc/transport/web.authenticationExempt"

func initializedTenantPlugin(t *testing.T, mutate func(*Config), options ...Option) *Plugin {
	t.Helper()
	cfg := defaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	p := newPlugin(cfg, options...)
	if err := p.Init(testContext()); err != nil {
		t.Fatal(err)
	}
	return p
}

func serveTenantRequest(p *Plugin, route web.RouteInfo, principal *authentication.Principal, headers http.Header, handler web.Handler) *httptest.ResponseRecorder {
	return serveTenantRequestWithExempt(p, route, principal, headers, false, handler)
}

// serveExemptTenantRequest drives a request the same way serveTenantRequest
// does, but also marks it exempt the way the authentication middleware would
// for a permit route -- see web.AuthenticationExempt.
func serveExemptTenantRequest(p *Plugin, route web.RouteInfo, principal *authentication.Principal, headers http.Header, handler web.Handler) *httptest.ResponseRecorder {
	return serveTenantRequestWithExempt(p, route, principal, headers, true, handler)
}

func serveTenantRequestWithExempt(p *Plugin, route web.RouteInfo, principal *authentication.Principal, headers http.Header, exempt bool, handler web.Handler) *httptest.ResponseRecorder {
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			if principal != nil {
				web.SetPrincipal(c, *principal)
			}
			if exempt {
				c.Set(authenticationExemptKeyForTest, true)
			}
			c.Next()
			return nil
		},
		p.Handler(),
		handler,
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(route.Method, route.Path, nil)
	request.Header = headers.Clone()
	engine.ServeHTTP(recorder, request)
	return recorder
}

// noContent is the do-nothing route handler shared by the cases whose subject
// is the middleware's decision rather than the response body.
func noContent(_ context.Context, c *web.Ctx) error {
	c.Status(http.StatusNoContent)
	return nil
}

func TestMiddlewarePublishesVerifiedTenantAndDefensiveAttributes(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := authentication.Principal{Subject: "alice", AuthMethod: "session", Attributes: map[string]any{
		"tenant_ids": []string{"acme", "beta"},
		"tenant_attributes": map[string]any{
			"beta": map[string]any{"plan": "enterprise", "nested": map[string]any{"region": "cn"}},
		},
	}}
	headers := make(http.Header)
	headers.Set(defaultHeader, "beta")
	response := serveTenantRequest(p, route, &principal, headers, func(_ context.Context, c *web.Ctx) error {
		resolved, ok := Current(c)
		if !ok || resolved.ID != "beta" || resolved.Attributes["plan"] != "enterprise" {
			c.Status(http.StatusInternalServerError)
			return nil
		}
		resolved.Attributes["nested"].(map[string]any)["region"] = "mutated"
		again, _ := Current(c)
		if again.Attributes["nested"].(map[string]any)["region"] != "cn" {
			c.Status(http.StatusInternalServerError)
			return nil
		}
		c.Status(http.StatusNoContent)
		return nil
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestMiddlewareNeverTrustsAnonymousOrNonMemberHeader pins that a request with
// no published Principal never has a tenant resolved from its header. A
// request with no principal on a real route is refused outright (see
// TestMiddlewareRefusesRealRouteWithoutPrincipal); it is the non-member case
// below -- a published principal that is simply not a member of the
// requested tenant -- that this test exists to pin, since an implementation
// that resolved tenancy from the header without checking membership first
// would leak Current(c) here and fail with 500 instead of 403.
func TestMiddlewareNeverTrustsAnonymousOrNonMemberHeader(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	headers := make(http.Header)
	headers.Set(defaultHeader, "admin")

	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme"}}}
	nonMember := serveTenantRequest(p, route, principal, headers, noContent)
	assertTenantError(t, nonMember, http.StatusForbidden, "forbidden")
}

// TestMiddlewareRefusesRealRouteWithoutPrincipal pins the security fix this
// task introduces: a real route (CurrentRoute hits) with no published
// Principal and no exemption cannot resolve a tenant and must be refused,
// never silently passed through. Before this fix, "no Principal" on any route
// -- real or unmatched -- fell through to c.Next(), with the comment arguing
// only 404/405 requests could reach that branch. AcceptedWithoutPrincipal
// broke that argument: a trusted-but-subjectless caller is authenticated, not
// exempt, and would otherwise bypass tenant isolation entirely.
func TestMiddlewareRefusesRealRouteWithoutPrincipal(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}

	uninitialized := New()
	assertTenantError(t, serveTenantRequest(uninitialized, route, nil, nil, noContent), http.StatusForbidden, "forbidden")

	p := initializedTenantPlugin(t, nil)
	assertTenantError(t, serveTenantRequest(p, route, nil, nil, noContent), http.StatusForbidden, "forbidden")
}

// TestMiddlewarePassesUnmatchedRouteWithoutPrincipal is the 404/405 regression
// lock the pre-fix comment described: CurrentRoute reports no match, so there
// is no handler behind the request to protect, and tenant must leave the
// response to the engine's own 404/405 instead of manufacturing a 403 for a
// route that does not exist.
func TestMiddlewarePassesUnmatchedRouteWithoutPrincipal(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}

	engine := enginetest.New()
	// No currentRouteKeyForTest stand-in: CurrentRoute misses, exactly as it
	// does for a request the engine resolves through its NoRoute chain.
	engine.Use(p.Handler())
	engine.GET(route.Path, noContent)
	engine.NoRoute([]web.Handler{p.Handler(), func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusNotFound)
		return nil
	}})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want %d from the engine's NoRoute chain", recorder.Code, recorder.Body.String(), http.StatusNotFound)
	}
}

// TestMiddlewarePassesOnMissingPrincipalButEnforces403OnMembership pins Ruling
// 4: authentication is no longer tenant's job to backstop. An authenticated
// request that fails tenant membership still gets tenant's own 403, exactly
// like a request with no principal at all on a real route (see
// TestMiddlewareRefusesRealRouteWithoutPrincipal) -- both are refused, just
// for different reasons.
func TestMiddlewarePassesOnMissingPrincipalButEnforces403OnMembership(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}

	noMembership := &authentication.Principal{Subject: "alice", AuthMethod: "jwt"}
	assertTenantError(t, serveTenantRequest(p, route, noMembership, nil, noContent), http.StatusForbidden, "forbidden")

	malformedPrincipal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []any{"acme", 9}}}
	assertTenantError(t, serveTenantRequest(p, route, malformedPrincipal, nil, noContent), http.StatusForbidden, "forbidden")
}

func TestMiddlewareRejectsAmbiguousAndMaliciousHeaders(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme", "beta"}}}
	cases := map[string][]string{
		"empty":      {""},
		"leading":    {" acme"},
		"trailing":   {"acme "},
		"comma":      {"acme,beta"},
		"path":       {"../admin"},
		"unicode":    {"tënänt"},
		"too long":   {strings.Repeat("a", 129)},
		"duplicates": {"acme", "beta"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range values {
				headers.Add(defaultHeader, value)
			}
			response := serveTenantRequest(p, route, principal, headers, noContent)
			assertTenantError(t, response, http.StatusForbidden, "forbidden")
		})
	}
}

// TestMiddlewareRejectsMalformedIDEvenWhenPrincipalListsIt separates the
// request-scoped ID-format guard from membership. The principal below lists the
// malformed value as one of its own members, so membership cannot be what
// rejects the request: only validTenantID on the requested header value can.
// A middleware that skipped that guard -- or relaxed it to admit non-ASCII
// bytes -- would resolve "tënänt" as a member and answer 204 instead of 403.
// Without this case every rejection in the table above is over-determined by
// both guards at once, so mutating either one alone would still pass.
func TestMiddlewareRejectsMalformedIDEvenWhenPrincipalListsIt(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme", "tënänt"}}}
	headers := make(http.Header)
	headers.Set(defaultHeader, "tënänt")
	assertTenantError(t, serveTenantRequest(p, route, principal, headers, noContent), http.StatusForbidden, "forbidden")
}

// TestMiddlewareExemptRouteBypassesTenantResolution pins that
// web.AuthenticationExempt, not the route's own .Auth() declaration, is what
// lets a request skip tenant resolution. It supplies both a principal and a
// malicious tenant header to prove the bypass is unconditional once exempt is
// set, matching what the authentication middleware does for a genuinely
// permitted route.
func TestMiddlewareExemptRouteBypassesTenantResolution(t *testing.T) {
	uninitialized := New()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/health"}
	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme"}}}
	headers := make(http.Header)
	headers.Set(defaultHeader, "../../attacker")
	response := serveExemptTenantRequest(uninitialized, route, principal, headers, func(_ context.Context, c *web.Ctx) error {
		if _, ok := Current(c); ok {
			c.Status(http.StatusInternalServerError)
			return nil
		}
		c.Status(http.StatusNoContent)
		return nil
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("exempt status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestRouteDeclaredPublicButNotExemptStillEnforcesMembership pins the
// vulnerability Ruling 20 closed: a route's own .Auth(Public()) declaration
// is only a tier-2 signal. When an application rule in web.security tightens
// that route for this request, the authentication middleware does not mark
// the request exempt, and tenant must not fall back to the route's
// IsPublic() declaration -- it must still enforce membership. An
// implementation that asked route.Auth.IsPublic() instead of
// web.AuthenticationExempt would let this request through with 204 instead
// of 403.
func TestRouteDeclaredPublicButNotExemptStillEnforcesMembership(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	publicPolicy := web.Public()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/login", Auth: &publicPolicy}
	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme"}}}
	headers := make(http.Header)
	headers.Set(defaultHeader, "attacker")
	response := serveTenantRequest(p, route, principal, headers, noContent)
	assertTenantError(t, response, http.StatusForbidden, "forbidden")
}

func TestMiddlewareOptionalTenantAllowsMissingMembership(t *testing.T) {
	optional := initializedTenantPlugin(t, func(cfg *Config) { cfg.Required = false })
	principal := &authentication.Principal{Subject: "alice"}
	response := serveTenantRequest(optional, web.RouteInfo{Method: http.MethodGet, Path: "/optional"}, principal, nil, func(_ context.Context, c *web.Ctx) error {
		if _, ok := Current(c); ok {
			c.Status(http.StatusInternalServerError)
			return nil
		}
		c.Status(http.StatusNoContent)
		return nil
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("optional status=%d body=%s", response.Code, response.Body.String())
	}

	badHeader := make(http.Header)
	badHeader.Set(defaultHeader, "attacker")
	assertTenantError(t, serveTenantRequest(optional, web.RouteInfo{Method: http.MethodGet, Path: "/optional"}, principal, badHeader, noContent), http.StatusForbidden, "forbidden")
}

func TestCustomResolverIsTrustedButMustMatchRequestedSelection(t *testing.T) {
	var calls atomic.Int32
	resolver := ResolverFunc(func(ctx context.Context, principal authentication.Principal, requested string) (Tenant, bool, error) {
		calls.Add(1)
		if ctx.Err() != nil || principal.Subject != "alice" {
			return Tenant{}, false, ctx.Err()
		}
		if requested == "external" {
			return Tenant{ID: "external", Attributes: map[string]any{"source": "database"}}, true, nil
		}
		return Tenant{}, false, nil
	})
	p := initializedTenantPlugin(t, nil, WithResolver(resolver))
	principal := &authentication.Principal{Subject: "alice"}
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	headers := make(http.Header)
	headers.Set(defaultHeader, "external")
	response := serveTenantRequest(p, route, principal, headers, func(_ context.Context, c *web.Ctx) error {
		resolved, ok := Current(c)
		if !ok || resolved.ID != "external" || resolved.Attributes["source"] != "database" {
			c.Status(http.StatusInternalServerError)
			return nil
		}
		c.Status(http.StatusNoContent)
		return nil
	})
	if response.Code != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}

	mismatch := initializedTenantPlugin(t, nil, WithResolver(ResolverFunc(func(context.Context, authentication.Principal, string) (Tenant, bool, error) {
		return Tenant{ID: "different"}, true, nil
	})))
	assertTenantError(t, serveTenantRequest(mismatch, route, principal, headers, noContent), http.StatusForbidden, "forbidden")
}

func TestConcurrentRequestsDoNotLeakTenantContextOrTrustMaliciousHeaders(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := &authentication.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme", "beta"}}}
	const requests = 100
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			headers := make(http.Header)
			want := "acme"
			status := http.StatusNoContent
			if index%3 == 1 {
				want = "beta"
			} else if index%3 == 2 {
				want = "attacker"
				status = http.StatusForbidden
			}
			headers.Set(defaultHeader, want)
			response := serveTenantRequest(p, route, principal, headers, func(_ context.Context, c *web.Ctx) error {
				resolved, ok := Current(c)
				if !ok || resolved.ID != want {
					c.Status(http.StatusInternalServerError)
					return nil
				}
				c.Status(http.StatusNoContent)
				return nil
			})
			if response.Code != status {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("concurrent request failures=%d", failures.Load())
	}
}

func assertTenantError(t *testing.T, response *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if response.Code != status || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("response=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != status || problem.Properties["code"] != message {
		t.Fatalf("problem=%#v, want status=%d code=%q", problem, status, message)
	}
}
