package casbin

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
	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

// authenticationExemptKeyForTest mirrors the private request-scoped key the
// authentication middleware sets when it resolves a request to permit (see
// web.AuthenticationExempt). Tests that exercise casbin standalone, without
// the real authentication middleware in front of it, set this key directly
// to simulate an exempt request -- mirroring the existing currentRouteKeyForTest
// convention above.
const authenticationExemptKeyForTest = "xbc/transport/web.authenticationExempt"

func TestAuthorizationUsesCurrentRoutePermissionAndPrincipal(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)

	allowed := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"},
		func(c *web.Ctx) {
			web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "jwt"})
		},
	)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d, body = %s", allowed.Code, allowed.Body.String())
	}

	denied := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"},
		func(c *web.Ctx) {
			web.SetPrincipal(c, authentication.Principal{Subject: "bob", AuthMethod: "jwt"})
		},
	)
	assertForbidden(t, denied)
}

func TestExemptRouteBypassesSubjectAndPolicy(t *testing.T) {
	p, _ := initializedPlugin(t, nil, nil)
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/login"},
		func(c *web.Ctx) { c.Set(authenticationExemptKeyForTest, true) },
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
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
	publicPolicy := web.Public()
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/login", Auth: &publicPolicy, Perm: "reports:read"},
		func(c *web.Ctx) { web.SetPrincipal(c, authentication.Principal{Subject: "bob"}) },
	)
	assertForbidden(t, response)
}

func TestProtectedRoutesFailClosed(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
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
			var before func(*web.Ctx)
			if test.principal {
				before = func(c *web.Ctx) { web.SetPrincipal(c, authentication.Principal{Subject: "alice"}) }
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
	p, _ := initializedPlugin(t, nil, func(cfg *Config) {
		cfg.MissingPermission = MissingPermissionAllow
	})
	response := requestThroughOptionalRoute(p, nil, func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	})
	assertForbidden(t, response)
}

func TestMissingPermissionCanBeExplicitlyAllowedAfterAuthentication(t *testing.T) {
	p, _ := initializedPlugin(t, nil, func(cfg *Config) {
		cfg.MissingPermission = MissingPermissionAllow
	})
	route := web.RouteInfo{Method: http.MethodGet, Path: "/profile"}
	withPrincipal := requestThroughCasbin(p, route, func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	})
	if withPrincipal.Code != http.StatusNoContent {
		t.Fatalf("explicit allow status = %d, body = %s", withPrincipal.Code, withPrincipal.Body.String())
	}
	assertForbidden(t, requestThroughCasbin(p, route, nil))
}

func TestPathMethodConventionUsesRouteTemplateAndMethod(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.RequestConvention = casbincore.ConventionPathMethod
		cfg.Policy = "p, alice, /reports/:id, GET"
	}, nil)
	response := requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodGet, Path: "/reports/:id"},
		func(c *web.Ctx) { web.SetPrincipal(c, authentication.Principal{Subject: "alice"}) },
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("path_method status = %d, body = %s", response.Code, response.Body.String())
	}
	assertForbidden(t, requestThroughCasbin(p,
		web.RouteInfo{Method: http.MethodDelete, Path: "/reports/:id"},
		func(c *web.Ctx) { web.SetPrincipal(c, authentication.Principal{Subject: "alice"}) },
	))
}

func TestInjectedResolverDoesNotRequireJWTOrPrincipal(t *testing.T) {
	var calls atomic.Int32
	resolver := SubjectResolverFunc(func(*web.Ctx) (string, bool) {
		calls.Add(1)
		return "service-account", true
	})
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, service-account, jobs:run"
	}, nil, WithSubjectResolver(resolver))
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

// TestEngineStoppedFailsClosedWhileMiddlewareIsLive pins the provider-side
// shutdown path. The engine and the middleware have independent lifecycles: a
// service may stop enforcing HTTP routes while rbac.Backend callers elsewhere
// are still shutting down, and the engine is then inactive first. Enforcer()
// reports that, and the middleware must deny every matched route rather than
// enforce against a torn-down policy source.
func TestEngineStoppedFailsClosedWhileMiddlewareIsLive(t *testing.T) {
	p, engine := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	principal := func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	}

	if response := requestThroughCasbin(p, route, principal); response.Code != http.StatusNoContent {
		t.Fatalf("pre-Stop status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("engine Stop() error = %v", err)
	}
	if _, active := engine.Enforcer(); active {
		t.Fatal("engine.Enforcer() still reports an active enforcer after Stop")
	}
	assertForbidden(t, requestThroughCasbin(p, route, principal))
}

// TestDisagreeingProviderDeclarationFailsClosed pins the cost of trusting a
// provider's declared convention. A provider that declares path_method while
// its enforcer's model has two request fields makes every Enforce call fail
// with a request-size error; the middleware logs it and denies. Trusting the
// declaration is deliberate -- verifying it by reading the model would
// reintroduce the reload race the provider contract exists to remove -- so the
// property this test pins is that such a provider can only ever deny, never
// admit a request or panic. The same engine and policy, reached through an
// honest provider, is admitted, which is what makes the denial meaningful.
func TestDisagreeingProviderDeclarationFailsClosed(t *testing.T) {
	engineCfg := casbincore.DefaultConfig()
	engineCfg.Policy = "p, alice, reports:read"
	engine, err := casbincore.New(engineCfg)
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })
	enforcer, active := engine.Enforcer()
	if !active || enforcer == nil {
		t.Fatal("engine did not hand out an enforcer")
	}
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	principal := func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	}

	honest, err := New(foreignProvider{enforcer: enforcer, active: true, convention: casbincore.ConventionRoutePermission}, DefaultConfig())
	if err != nil {
		t.Fatalf("New(honest provider) error = %v", err)
	}
	if response := requestThroughCasbin(honest, route, principal); response.Code != http.StatusNoContent {
		t.Fatalf("honest provider status = %d, body = %s", response.Code, response.Body.String())
	}

	lying, err := New(foreignProvider{enforcer: enforcer, active: true, convention: casbincore.ConventionPathMethod}, DefaultConfig())
	if err != nil {
		t.Fatalf("New(lying provider) error = %v", err)
	}
	assertForbidden(t, requestThroughCasbin(lying, route, principal))
}

// TestRequestPathDoesNotReadTheEnforcerModel is the -race guard for the
// convention lookup. SyncedEnforcer.GetModel returns the model field that
// LoadPolicy replaces under its own lock, so resolving the convention from the
// model on every request races any concurrent reload: the engine's periodic
// reload, a watcher notification, or an explicit LoadPolicy such as the one
// driven here. The middleware instead takes the convention once, at
// construction, from the provider that validated it; with the model read
// removed from authorize, this test observes only completed policy states.
// Reintroducing a per-request GetModel() call makes the race detector flag the
// reload goroutine against the request loop -- so this guard is meaningful
// under -race (make test-race); without it the test only re-checks that
// allow/deny outcomes stay stable while the policy reloads.
func TestRequestPathDoesNotReadTheEnforcerModel(t *testing.T) {
	p, engine := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	principal := func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	}
	other := func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "bob"})
	}

	stopReload := make(chan struct{})
	reloadErr := make(chan error, 1)
	var reloads sync.WaitGroup
	reloads.Add(1)
	go func() {
		defer reloads.Done()
		for {
			select {
			case <-stopReload:
				return
			default:
			}
			if err := engine.LoadPolicy(); err != nil {
				reloadErr <- err
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		allowed := requestThroughCasbin(p, route, principal)
		if allowed.Code != http.StatusNoContent {
			t.Fatalf("request %d: allowed status = %d, body = %s", i, allowed.Code, allowed.Body.String())
		}
		assertForbidden(t, requestThroughCasbin(p, route, other))
	}
	close(stopReload)
	reloads.Wait()
	select {
	case err := <-reloadErr:
		t.Fatalf("engine.LoadPolicy() error = %v", err)
	default:
	}
}

func TestDefaultResolverIgnoresUnverifiedAuthorizationHeader(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, attacker, reports:read"
	}, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	response := requestThroughOptionalRouteWithHeader(p, &route, nil, "Bearer unverified.attacker.token")
	assertForbidden(t, response)
}

func TestMiddlewareReadsRouteMetadataPerRequest(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
	principal := func(c *web.Ctx) { web.SetPrincipal(c, authentication.Principal{Subject: "alice"}) }

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

// TestStopFailsClosedAndLeavesTheEngineRunning pins the split's shutdown
// contract from the middleware side: after Stop every matched route is denied
// from the first request on, and the engine the middleware consumed is still
// active -- rbac.Backend callers and other consumers may outlive this
// middleware, so its Stop must not deactivate the enforcer it only borrowed.
func TestStopFailsClosedAndLeavesTheEngineRunning(t *testing.T) {
	p, engine := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, alice, reports:read"
	}, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/reports", Perm: "reports:read"}
	principal := func(c *web.Ctx) {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice"})
	}

	if response := requestThroughCasbin(p, route, principal); response.Code != http.StatusNoContent {
		t.Fatalf("pre-Stop status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	assertForbidden(t, requestThroughCasbin(p, route, principal))
	if _, ok := engine.Enforcer(); !ok {
		t.Fatal("the middleware's Stop must not deactivate the engine it consumes")
	}
}

func requestThroughCasbin(p *Plugin, route web.RouteInfo, before func(*web.Ctx)) *httptest.ResponseRecorder {
	return requestThroughOptionalRoute(p, &route, before)
}

func requestThroughOptionalRoute(p *Plugin, route *web.RouteInfo, before func(*web.Ctx)) *httptest.ResponseRecorder {
	return requestThroughOptionalRouteWithHeader(p, route, before, "")
}

func requestThroughOptionalRouteWithHeader(p *Plugin, route *web.RouteInfo, before func(*web.Ctx), authorization string) *httptest.ResponseRecorder {
	method, path := http.MethodGet, "/test"
	if route != nil {
		method, path = route.Method, route.Path
	}
	engine := enginetest.New()
	engine.Handle(method, muxPattern(path), []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			if route != nil {
				c.Set(currentRouteKeyForTest, *route)
			}
			if before != nil {
				before(c)
			}
			c.Next()
			return nil
		},
		p.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			c.Status(http.StatusNoContent)
			return nil
		},
	})

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

// muxPattern rewrites the gin-style ":id" wildcard these RouteInfo values
// carry into the ServeMux "{id}" form the test engine registers with. Only the
// registered pattern is translated: the RouteInfo the middleware reads keeps
// its original path, because that string is the permission subject under the
// path_method convention.
func muxPattern(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[i] = "{" + segment[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// TestPluginDoesNotDeclareRequiresPrincipal locks the decision in Order's doc
// comment: *Plugin must not implement authentication.RequiresPrincipal.
// casbin's actual identity dependency is SubjectResolver, which an injected
// resolver may satisfy from a source other than authentication.Principal
// entirely (see TestInjectedResolverDoesNotRequireJWTOrPrincipal); declaring
// the marker would wrongly couple every casbin configuration, including one
// using a custom resolver, to Web's Principal contract. If a future change
// makes *Plugin implement RequiresPrincipal, this test is the one to update
// deliberately alongside the Order doc comment and casbin's package doc.
func TestPluginDoesNotDeclareRequiresPrincipal(t *testing.T) {
	var value any = (*Plugin)(nil)
	if _, ok := value.(authentication.RequiresPrincipal); ok {
		t.Fatal("*Plugin must not implement authentication.RequiresPrincipal; see Order's doc comment")
	}
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
