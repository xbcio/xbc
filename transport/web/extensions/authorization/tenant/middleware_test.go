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

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

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

func serveTenantRequest(p *Plugin, route web.RouteInfo, principal *web.Principal, headers http.Header, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, route)
		if principal != nil {
			web.SetPrincipal(c, *principal)
		}
		c.Next()
	})
	engine.Use(p.Handler())
	engine.Handle(route.Method, route.Path, handler)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(route.Method, route.Path, nil)
	request.Header = headers.Clone()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestMiddlewarePublishesVerifiedTenantAndDefensiveAttributes(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := web.Principal{Subject: "alice", AuthMethod: "session", Attributes: map[string]any{
		"tenant_ids": []string{"acme", "beta"},
		"tenant_attributes": map[string]any{
			"beta": map[string]any{"plan": "enterprise", "nested": map[string]any{"region": "cn"}},
		},
	}}
	headers := make(http.Header)
	headers.Set(defaultHeader, "beta")
	response := serveTenantRequest(p, route, &principal, headers, func(c *gin.Context) {
		resolved, ok := Current(c)
		if !ok || resolved.ID != "beta" || resolved.Attributes["plan"] != "enterprise" {
			c.Status(http.StatusInternalServerError)
			return
		}
		resolved.Attributes["nested"].(map[string]any)["region"] = "mutated"
		again, _ := Current(c)
		if again.Attributes["nested"].(map[string]any)["region"] != "cn" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMiddlewareNeverTrustsAnonymousOrNonMemberHeader(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	headers := make(http.Header)
	headers.Set(defaultHeader, "admin")
	anonymous := serveTenantRequest(p, route, nil, headers, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	assertTenantError(t, anonymous, http.StatusUnauthorized, "unauthorized")

	principal := &web.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme"}}}
	nonMember := serveTenantRequest(p, route, principal, headers, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	assertTenantError(t, nonMember, http.StatusForbidden, "forbidden")
}

func TestMiddlewareUsesUniform401And403Responses(t *testing.T) {
	uninitialized := New()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	assertTenantError(t, serveTenantRequest(uninitialized, route, nil, nil, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusUnauthorized, "unauthorized")

	p := initializedTenantPlugin(t, nil)
	assertTenantError(t, serveTenantRequest(p, route, nil, nil, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusUnauthorized, "unauthorized")

	noMembership := &web.Principal{Subject: "alice", AuthMethod: "jwt"}
	assertTenantError(t, serveTenantRequest(p, route, noMembership, nil, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusForbidden, "forbidden")

	malformedPrincipal := &web.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []any{"acme", 9}}}
	assertTenantError(t, serveTenantRequest(p, route, malformedPrincipal, nil, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusForbidden, "forbidden")
}

func TestMiddlewareRejectsAmbiguousAndMaliciousHeaders(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := &web.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme", "beta"}}}
	cases := map[string][]string{
		"empty":      {""},
		"leading":    {" acme"},
		"trailing":   {"acme "},
		"comma":      {"acme,beta"},
		"path":       {"../admin"},
		"unicode":    {"租户"},
		"too long":   {strings.Repeat("a", 129)},
		"duplicates": {"acme", "beta"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range values {
				headers.Add(defaultHeader, value)
			}
			response := serveTenantRequest(p, route, principal, headers, func(c *gin.Context) { c.Status(http.StatusNoContent) })
			assertTenantError(t, response, http.StatusForbidden, "forbidden")
		})
	}
}

func TestMiddlewarePublicBypassAndOptionalTenant(t *testing.T) {
	uninitialized := New()
	publicPolicy := web.Public()
	public := web.RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &publicPolicy}
	headers := make(http.Header)
	headers.Set(defaultHeader, "../../attacker")
	response := serveTenantRequest(uninitialized, public, nil, headers, func(c *gin.Context) {
		if _, ok := Current(c); ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("public status=%d body=%s", response.Code, response.Body.String())
	}

	optional := initializedTenantPlugin(t, func(cfg *Config) { cfg.Required = false })
	principal := &web.Principal{Subject: "alice"}
	response = serveTenantRequest(optional, web.RouteInfo{Method: http.MethodGet, Path: "/optional"}, principal, nil, func(c *gin.Context) {
		if _, ok := Current(c); ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("optional status=%d body=%s", response.Code, response.Body.String())
	}

	badHeader := make(http.Header)
	badHeader.Set(defaultHeader, "attacker")
	assertTenantError(t, serveTenantRequest(optional, web.RouteInfo{Method: http.MethodGet, Path: "/optional"}, principal, badHeader, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusForbidden, "forbidden")
}

func TestCustomResolverIsTrustedButMustMatchRequestedSelection(t *testing.T) {
	var calls atomic.Int32
	resolver := ResolverFunc(func(ctx context.Context, principal web.Principal, requested string) (Tenant, bool, error) {
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
	principal := &web.Principal{Subject: "alice"}
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	headers := make(http.Header)
	headers.Set(defaultHeader, "external")
	response := serveTenantRequest(p, route, principal, headers, func(c *gin.Context) {
		resolved, ok := Current(c)
		if !ok || resolved.ID != "external" || resolved.Attributes["source"] != "database" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}

	mismatch := initializedTenantPlugin(t, nil, WithResolver(ResolverFunc(func(context.Context, web.Principal, string) (Tenant, bool, error) {
		return Tenant{ID: "different"}, true, nil
	})))
	assertTenantError(t, serveTenantRequest(mismatch, route, principal, headers, func(c *gin.Context) { c.Status(http.StatusNoContent) }), http.StatusForbidden, "forbidden")
}

func TestConcurrentRequestsDoNotLeakTenantContextOrTrustMaliciousHeaders(t *testing.T) {
	p := initializedTenantPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	principal := &web.Principal{Subject: "alice", Attributes: map[string]any{"tenant_ids": []string{"acme", "beta"}}}
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
			response := serveTenantRequest(p, route, principal, headers, func(c *gin.Context) {
				resolved, ok := Current(c)
				if !ok || resolved.ID != want {
					c.Status(http.StatusInternalServerError)
					return
				}
				c.Status(http.StatusNoContent)
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
