package casbin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

// authenticationExemptKeyForTest mirrors the private gin.Context key the
// authentication middleware sets when it resolves a request to permit (see
// web.AuthenticationExempt). Tests that exercise casbin standalone, without
// the real authentication middleware in front of it, set this key directly
// to simulate an exempt request -- mirroring the existing currentRouteKeyForTest
// convention above.
const authenticationExemptKeyForTest = "xbc/transport/web.authenticationExempt"

func init() {
	gin.SetMode(gin.TestMode)
}

func TestAuthorizationUsesCurrentRoutePermissionAndPrincipal(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, alice, reports:read"
	})

	allowed := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"},
		func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "alice", AuthMethod: "jwt"}) },
	)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d, body = %s", allowed.Code, allowed.Body.String())
	}

	denied := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"},
		func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "bob", AuthMethod: "jwt"}) },
	)
	assertForbidden(t, denied)
}

func TestExemptRouteBypassesSubjectAndPolicy(t *testing.T) {
	p, _ := initializedPlugin(t, nil)
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/login"},
		func(c *gin.Context) { c.Set(authenticationExemptKeyForTest, true) },
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("exempt status = %d, body = %s", response.Code, response.Body.String())
	}
}

// TestRouteDeclaredPublicButNotExemptStillEnforces pins the vulnerability
// Ruling 20 closed: a route's own .Auth(Public()) declaration is only a
// tier-2 signal. When an application rule in web.security tightens that
// route for this request, the authentication middleware does not mark the
// request exempt, and casbin must not fall back to the route's IsPublic()
// declaration -- it must still enforce. An implementation that asked
// route.Auth.IsPublic() instead of web.AuthenticationExempt would let this
// request through with 204 instead of 403.
func TestRouteDeclaredPublicButNotExemptStillEnforces(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, alice, reports:read"
	})
	publicPolicy := web.Public()
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/login", Auth: &publicPolicy, Perm: "reports:read"},
		func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "bob"}) },
	)
	assertForbidden(t, response)
}

func TestProtectedRoutesFailClosed(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, alice, reports:read"
	})
	tests := []struct {
		name      string
		route     *web.RouteInfo
		principal bool
	}{
		{name: "missing CurrentRoute", route: nil, principal: true},
		{name: "missing Principal", route: &web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}},
		{name: "missing Perm", route: &web.RouteInfo{Method: http.MethodGet, Path: "/reports"}, principal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var before gin.HandlerFunc
			if test.principal {
				before = func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "alice"}) }
			}
			response := requestThroughOptionalRoute(p, test.route, before)
			assertForbidden(t, response)
		})
	}
}

// TestMissingCurrentRouteFailsClosedEvenWhenMissingPermissionIsAllowed pins
// the `!found` guard on CurrentRoute independently of TestProtectedRoutesFailClosed's
// "missing Perm" subtest. Deleting the guard (using `route, _ :=
// web.CurrentRoute(c)` and ignoring `found`) still 403s the "missing
// CurrentRoute" subtest there, because the resulting zero-value RouteInfo has
// an empty Perm, which under the default MissingPermissionDeny falls through
// to the same forbidden() call the "missing Perm" subtest already exercises --
// so that subtest alone cannot prove the guard was ever there. Combining
// MissingPermissionAllow with the default ConventionRoutePermission and no
// CurrentRoute isolates it: with the guard deleted, an empty Perm on
// MissingPermissionAllow takes the c.Next() branch and this request would
// incorrectly succeed with 204 instead of 403.
func TestMissingCurrentRouteFailsClosedEvenWhenMissingPermissionIsAllowed(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.MissingPermission = MissingPermissionAllow
	})
	response := requestThroughOptionalRoute(p, nil, func(c *gin.Context) {
		web.SetPrincipal(c, web.Principal{Subject: "alice"})
	})
	assertForbidden(t, response)
}

func TestMissingPermissionCanBeExplicitlyAllowedAfterAuthentication(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.MissingPermission = MissingPermissionAllow
	})
	route := web.RouteInfo{Method: http.MethodGet, Path: "/profile"}
	withPrincipal := requestThroughCasbin(p, route, func(c *gin.Context) {
		web.SetPrincipal(c, web.Principal{Subject: "alice"})
	})
	if withPrincipal.Code != http.StatusNoContent {
		t.Fatalf("explicit allow status = %d, body = %s", withPrincipal.Code, withPrincipal.Body.String())
	}
	assertForbidden(t, requestThroughCasbin(p, route, nil))
}

func TestPathMethodConventionUsesRouteTemplateAndMethod(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.RequestConvention = ConventionPathMethod
		cfg.Policy = "p, alice, /reports/:id, GET"
	})
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports/:id"},
		func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "alice"}) },
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("path_method status = %d, body = %s", response.Code, response.Body.String())
	}
	assertForbidden(t, requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodDelete, Path: "/reports/:id"},
		func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "alice"}) },
	))
}

func TestInjectedResolverDoesNotRequireJWTOrPrincipal(t *testing.T) {
	var calls atomic.Int32
	resolver := SubjectResolverFunc(func(*gin.Context) (string, bool) {
		calls.Add(1)
		return "service-account", true
	})
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, service-account, jobs:run"
	}, WithSubjectResolver(resolver))
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodPost, Path: "/jobs", Perm: "jobs:run"}, nil,
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("custom resolver status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls.Load())
	}
}

func TestDefaultResolverIgnoresUnverifiedAuthorizationHeader(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, attacker, reports:read"
	})
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	response := requestThroughOptionalRouteWithHeader(p, &route, nil, "Bearer unverified.attacker.token")
	assertForbidden(t, response)
}

func TestMiddlewareReadsRouteMetadataPerRequest(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Policy = "p, alice, reports:read"
	})
	principal := func(c *gin.Context) { web.SetPrincipal(c, web.Principal{Subject: "alice"}) }

	missingPerm := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports"}, principal,
	)
	assertForbidden(t, missingPerm)
	withPerm := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}, principal,
	)
	if withPerm.Code != http.StatusNoContent {
		t.Fatalf("second request status = %d, body = %s", withPerm.Code, withPerm.Body.String())
	}
}

func requestThroughCasbin(p *Plugin, route web.RouteInfo, before gin.HandlerFunc) *httptest.ResponseRecorder {
	return requestThroughOptionalRoute(p, &route, before)
}

func requestThroughOptionalRoute(p *Plugin, route *web.RouteInfo, before gin.HandlerFunc) *httptest.ResponseRecorder {
	return requestThroughOptionalRouteWithHeader(p, route, before, "")
}

func requestThroughOptionalRouteWithHeader(p *Plugin, route *web.RouteInfo, before gin.HandlerFunc, authorization string) *httptest.ResponseRecorder {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if route != nil {
			c.Set(currentRouteKeyForTest, *route)
		}
		if before != nil {
			before(c)
		}
		c.Next()
	})
	engine.Use(p.Handler())
	method, path := http.MethodGet, "/test"
	if route != nil {
		method, path = route.Method, route.Path
	}
	engine.Handle(method, path, func(c *gin.Context) { c.Status(http.StatusNoContent) })

	recorder := httptest.NewRecorder()
	requestPath := path
	if path == "/reports/:id" {
		requestPath = "/reports/42"
	}
	request := httptest.NewRequest(method, requestPath, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	engine.ServeHTTP(recorder, request)
	return recorder
}

func assertForbidden(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", recorder.Code, recorder.Body.String())
	}
	var body web.ProblemDetail
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode forbidden body: %v", err)
	}
	if body.Status != http.StatusForbidden || body.Properties["code"] != "forbidden" {
		t.Fatalf("problem = %#v, want forbidden", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if len(recorder.Header().Values("WWW-Authenticate")) != 0 {
		t.Fatal("authorization middleware must not issue an authentication challenge")
	}
}
