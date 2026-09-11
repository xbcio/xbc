package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

type stubAuth struct {
	scheme authentication.Scheme
	result authentication.Result
	// calls counts Authenticate invocations so a test can assert that a request
	// reached, or never reached, policy resolution.
	calls int
}

func (s *stubAuth) Scheme() authentication.Scheme { return s.scheme }

func (s *stubAuth) Authenticate(
	context.Context, authentication.Credential,
) (authentication.Result, error) {
	s.calls++
	return s.result, nil
}

// newTestCatalog builds a frozen catalog directly. routeCatalog is package
// private, so a test can construct one without driving a whole Router.
func newTestCatalog(routes []RouteInfo) RouteCatalog {
	index := make(map[string]RouteInfo, len(routes))
	for _, route := range routes {
		index[routeKey(route.Method, route.Path)] = route
	}
	return &routeCatalog{all: append([]RouteInfo(nil), routes...), index: index}
}

func setCurrentRouteForTest(c *gin.Context, route RouteInfo) {
	c.Set(currentRouteContextKey, route)
}

// buildTestAuthMiddleware wires a middleware over one scheme and one frozen
// route, mirroring what the Server does at startup.
func buildTestAuthMiddleware(
	t *testing.T,
	cfg SecurityConfig,
	routes []RouteInfo,
	extraction authentication.CredentialResult,
	authenticator *stubAuth,
) *authenticationMiddleware {
	t.Helper()
	middleware, err := newAuthenticationMiddleware(
		cfg,
		[]plugin.Entry[authentication.Authenticator]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    authenticator,
			},
		},
		[]plugin.Entry[CredentialExtractor]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    &stubExtractor{scheme: "jwt", result: extraction},
			},
		},
	)
	if err != nil {
		t.Fatalf("newAuthenticationMiddleware() error = %v", err)
	}
	if err := middleware.RoutesReady(newTestCatalog(routes)); err != nil {
		t.Fatalf("RoutesReady() error = %v", err)
	}
	return middleware
}

func runThroughAuth(
	t *testing.T,
	middleware *authenticationMiddleware,
	route RouteInfo,
	registerRoute bool,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if registerRoute {
			setCurrentRouteForTest(c, route)
		}
		c.Next()
	})
	engine.Use(middleware.Handler())
	engine.Handle(route.Method, route.Path, func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(route.Method, route.Path, nil))
	return recorder
}

func TestAuthenticationMiddlewarePassesUnmatchedRouteWithoutResolving(t *testing.T) {
	t.Parallel()

	// The compiled table is deliberately populated with a denying route: a
	// request that matched no frozen route must fall through before any lookup,
	// so an implementation that dropped the CurrentRoute check would land in the
	// uncompiled-route branch and answer 500 instead of gin's 404.
	route := RouteInfo{Method: http.MethodGet, Path: "/orders"}
	authenticator := &stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{route},
		authentication.Absent(),
		authenticator,
	)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// No recordCurrentRoute stand-in: CurrentRoute misses, exactly as it does
	// for a request gin resolves through allNoRoute.
	engine.Use(middleware.Handler())
	engine.Handle(route.Method, route.Path, func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d from gin's NoRoute handler", recorder.Code, http.StatusNotFound)
	}
	if authenticator.calls != 0 {
		t.Fatalf(
			"authenticator calls = %d, want 0: an unmatched route must not resolve a policy",
			authenticator.calls,
		)
	}
}

func TestAuthenticationMiddlewareFailsClosedOnUncompiledRoute(t *testing.T) {
	t.Parallel()

	// A recorded route that the compiled table does not know can only mean the
	// table is stale or was never built. That must fail closed, not release the
	// request unauthenticated.
	public := Public()
	authenticator := &stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{{Method: http.MethodGet, Path: "/health", Auth: &public}},
		authentication.Absent(),
		authenticator,
	)

	got := runThroughAuth(t, middleware, RouteInfo{Method: http.MethodGet, Path: "/orders"}, true)
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusInternalServerError)
	}
	if authenticator.calls != 0 {
		t.Fatalf("authenticator calls = %d, want 0", authenticator.calls)
	}
}

func TestAuthenticationMiddlewarePermitsPublicRoute(t *testing.T) {
	t.Parallel()

	public := Public()
	route := RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &public}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{route},
		authentication.Absent(),
		&stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")},
	)

	if got := runThroughAuth(t, middleware, route, true); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusOK)
	}
}

func TestAuthenticationMiddlewarePublishesPrincipalOnce(t *testing.T) {
	t.Parallel()

	route := RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{route},
		authentication.Presented("token"),
		&stubAuth{
			scheme: "jwt",
			result: authentication.Accepted(Principal{Subject: "u-1", AuthMethod: "jwt"}),
		},
	)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { setCurrentRouteForTest(c, route); c.Next() })
	engine.Use(middleware.Handler())

	var seen Principal
	var found bool
	engine.GET("/orders", func(c *gin.Context) {
		seen, found = CurrentPrincipal(c)
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/orders", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !found {
		t.Fatal("framework must publish the Principal after a successful authentication")
	}
	if seen.Subject != "u-1" || seen.AuthMethod != "jwt" {
		t.Fatalf("principal = %+v, want subject u-1 / method jwt", seen)
	}
}

func TestAuthenticationMiddlewareRejectsWithSeparateChallengeHeaders(t *testing.T) {
	t.Parallel()

	route := RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{route},
		authentication.AbsentWithChallenge(`Bearer realm="api"`),
		&stubAuth{scheme: "jwt", result: authentication.Rejected("unused")},
	)

	got := runThroughAuth(t, middleware, route, true)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusUnauthorized)
	}
	challenges := got.Header().Values("WWW-Authenticate")
	if len(challenges) != 1 || challenges[0] != `Bearer realm="api"` {
		t.Fatalf("WWW-Authenticate = %v, want one Bearer challenge", challenges)
	}
}

func TestAuthenticationMiddlewareRoutesReadyRejectsUnknownScheme(t *testing.T) {
	t.Parallel()

	manager, err := authentication.NewManager(authentication.ManagerOptions{
		Authenticators: []authentication.Authenticator{&stubAuth{scheme: "jwt"}},
		DefaultSchemes: []authentication.Scheme{"jwt"},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	set, err := newPolicySet(SecurityConfig{
		Policies: []PolicyRule{{
			Match:        "/api/**",
			Authenticate: []authentication.Scheme{"nope"},
		}},
	}.normalize())
	if err != nil {
		t.Fatalf("newPolicySet() error = %v", err)
	}
	middleware := &authenticationMiddleware{manager: manager, policies: set}

	err = middleware.RoutesReady(newTestCatalog([]RouteInfo{
		{Method: http.MethodGet, Path: "/api/v1/orders"},
	}))
	if err == nil {
		t.Fatal("RoutesReady() error = nil, want unknown scheme error")
	}
	// The message must name the offending scheme: an assertion on err != nil
	// alone would also accept a failure from an unrelated validation.
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("RoutesReady() error = %v, want it to name scheme \"nope\"", err)
	}
}

func TestAuthenticationMiddlewareRoutesReadyRejectsMissingAuthenticator(t *testing.T) {
	t.Parallel()

	set, err := newPolicySet(SecurityConfig{}.normalize())
	if err != nil {
		t.Fatalf("newPolicySet() error = %v", err)
	}
	middleware := &authenticationMiddleware{policies: set}

	err = middleware.RoutesReady(newTestCatalog([]RouteInfo{
		{Method: http.MethodGet, Path: "/orders"},
	}))
	if err == nil {
		t.Fatal("RoutesReady() error = nil, want missing authenticator error")
	}
	if !strings.Contains(err.Error(), "GET /orders") ||
		!strings.Contains(err.Error(), "no authenticator is registered") {
		t.Fatalf("RoutesReady() error = %v, want it to name the route and the cause", err)
	}
}

func TestAuthenticationMiddlewareRoutesReadyAllowsPermitOnlyServiceWithoutAuthenticator(t *testing.T) {
	t.Parallel()

	public := Public()
	set, err := newPolicySet(SecurityConfig{}.normalize())
	if err != nil {
		t.Fatalf("newPolicySet() error = %v", err)
	}
	middleware := &authenticationMiddleware{policies: set}

	if err := middleware.RoutesReady(newTestCatalog([]RouteInfo{
		{Method: http.MethodGet, Path: "/health", Auth: &public},
	})); err != nil {
		t.Fatalf("RoutesReady() error = %v, want nil", err)
	}
}

func TestAuthenticationMiddlewareReportsResolvedDecisions(t *testing.T) {
	t.Parallel()

	public := Public()
	routes := []RouteInfo{
		{Method: http.MethodGet, Path: "/health", Auth: &public},
		{Method: http.MethodGet, Path: "/orders"},
	}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		routes,
		authentication.Absent(),
		&stubAuth{scheme: "jwt", result: authentication.Rejected("unused")},
	)

	open := middleware.publicRoutes()
	if len(open) != 1 || open[0].Path != "/health" {
		t.Fatalf("publicRoutes() = %+v, want only /health", open)
	}
	decisions := middleware.decisions()
	if len(decisions) != len(routes) {
		t.Fatalf("decisions() length = %d, want %d", len(decisions), len(routes))
	}
	for _, decision := range decisions {
		switch decision.route.Path {
		case "/health":
			if !decision.policy.permit || decision.policy.tier != tierRoute {
				t.Fatalf("/health decision = %+v, want permit from the route tier", decision.policy)
			}
		case "/orders":
			if decision.policy.permit || decision.policy.tier != tierDefault {
				t.Fatalf("/orders decision = %+v, want deny from the default tier", decision.policy)
			}
		}
	}
}

// TestAuthenticationMiddlewarePublishesExemptSignalToDownstream is the
// producer-side counterpart to casbin's and tenant's exempt-consumer tests:
// until this test existed, nothing in the repository drove a request through
// the real authentication middleware and read AuthenticationExempt back from
// a downstream middleware sharing the same *gin.Context. casbin and tenant
// only ever simulated the flag by setting the context key directly, so a
// producer-side regression -- markAuthenticationExempt deleted, or hoisted to
// run unconditionally -- left every test in the repository green. Both
// mutations are opposite-direction production incidents: the first 403s every
// permitted route, the second bypasses casbin and tenant on every protected
// one.
func TestAuthenticationMiddlewarePublishesExemptSignalToDownstream(t *testing.T) {
	t.Parallel()

	public := Public()
	permitRoute := RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &public}
	denyRoute := RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		[]RouteInfo{permitRoute, denyRoute},
		authentication.Presented("token"),
		&stubAuth{
			scheme: "jwt",
			result: authentication.Accepted(Principal{Subject: "u-1", AuthMethod: "jwt"}),
		},
	)

	// buildEngine wires the real authentication middleware followed by a
	// downstream middleware standing in for a Require(AuthenticationMiddlewareKey)
	// consumer such as casbin or tenant: it reads AuthenticationExempt and
	// CurrentPrincipal from the same context the authentication middleware just
	// populated, exactly as those two extensions do in production.
	buildEngine := func(route RouteInfo, registerRoute bool) (engine *gin.Engine, exempt, hadPrincipal *bool) {
		gin.SetMode(gin.TestMode)
		exempt = new(bool)
		hadPrincipal = new(bool)
		engine = gin.New()
		engine.Use(func(c *gin.Context) {
			if registerRoute {
				setCurrentRouteForTest(c, route)
			}
			c.Next()
		})
		engine.Use(middleware.Handler())
		engine.Use(func(c *gin.Context) {
			*exempt = AuthenticationExempt(c)
			_, *hadPrincipal = CurrentPrincipal(c)
			c.Next()
		})
		engine.Handle(route.Method, route.Path, func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })
		return engine, exempt, hadPrincipal
	}

	t.Run("permit route", func(t *testing.T) {
		engine, exempt, _ := buildEngine(permitRoute, true)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(permitRoute.Method, permitRoute.Path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if !*exempt {
			t.Fatal("AuthenticationExempt() = false on a permit route, want true")
		}
	})

	t.Run("deny route authenticates", func(t *testing.T) {
		engine, exempt, hadPrincipal := buildEngine(denyRoute, true)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(denyRoute.Method, denyRoute.Path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if *exempt {
			t.Fatal("AuthenticationExempt() = true on an authenticated deny route, want false: " +
				"this would bypass casbin and tenant on every protected route")
		}
		if !*hadPrincipal {
			t.Fatal("CurrentPrincipal() missing after a successful authentication")
		}
	})

	t.Run("unmatched route", func(t *testing.T) {
		unmatched := RouteInfo{Method: http.MethodGet, Path: "/does-not-exist"}
		gin.SetMode(gin.TestMode)
		exempt := new(bool)
		hadPrincipal := new(bool)
		engine := gin.New()
		// No setCurrentRouteForTest stand-in: CurrentRoute misses, exactly as it
		// does for a request gin resolves through allNoRoute.
		engine.Use(middleware.Handler())
		engine.Use(func(c *gin.Context) {
			*exempt = AuthenticationExempt(c)
			_, *hadPrincipal = CurrentPrincipal(c)
			c.Next()
		})
		engine.Handle(permitRoute.Method, permitRoute.Path, func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusNotFound) })

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(unmatched.Method, unmatched.Path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
		}
		if *exempt {
			t.Fatal("AuthenticationExempt() = true on an unmatched route, want false")
		}
		if *hadPrincipal {
			t.Fatal("CurrentPrincipal() present on an unmatched route, want absent")
		}
	})
}

// TestAuthenticationExemptContextKeyIsStable pins the exact string literal
// markAuthenticationExempt writes and AuthenticationExempt reads. casbin and
// tenant cannot import the private authenticationExemptContextKey constant, so
// each mirrors it as its own hardcoded test-only literal
// (authenticationExemptKeyForTest) to simulate an exempt request without
// driving the real middleware. Those two mirrors do catch a drift in the
// constant's value, but nothing inside this package did: every call here goes
// through the exported functions, so both sides of a value change move
// together and stay green. Hardcoding the literal independently here closes
// that gap: a change to authenticationExemptContextKey's value now fails in
// this package too, not only in the two downstream modules.
func TestAuthenticationExemptContextKeyIsStable(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("xbc/transport/web.authenticationExempt", true)
	if !AuthenticationExempt(c) {
		t.Fatal("AuthenticationExempt() = false after setting the documented literal " +
			`"xbc/transport/web.authenticationExempt" directly, want true`)
	}
}

func TestAuthenticationMiddlewareOrderIsAuthPhase(t *testing.T) {
	t.Parallel()

	var middleware Middleware = &authenticationMiddleware{}
	if got := middleware.Order(); got.Phase != PhaseAuth {
		t.Fatalf("Order().Phase = %v, want %v", got.Phase, PhaseAuth)
	}
}
