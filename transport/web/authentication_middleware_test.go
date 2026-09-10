package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/plugin"
)

type stubAuth struct {
	scheme authentication.Scheme
	result authentication.Result
}

func (s *stubAuth) Scheme() authentication.Scheme { return s.scheme }

func (s *stubAuth) Authenticate(
	context.Context, authentication.Credential,
) (authentication.Result, error) {
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
// route, mirroring what the plugin definition does at startup.
func buildTestAuthMiddleware(
	t *testing.T,
	cfg SecurityConfig,
	routes []RouteInfo,
	extraction authentication.CredentialResult,
	outcome authentication.Result,
) *authenticationMiddleware {
	t.Helper()
	middleware, err := newAuthenticationMiddleware(
		cfg,
		[]plugin.Entry[authentication.Authenticator]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    &stubAuth{scheme: "jwt", result: outcome},
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

	route := RouteInfo{Method: http.MethodGet, Path: "/missing"}
	middleware := buildTestAuthMiddleware(
		t,
		SecurityConfig{Default: SecurityDeny},
		nil,
		authentication.Absent(),
		authentication.Rejected("should not run"),
	)

	got := runThroughAuth(t, middleware, route, false)
	if got.Code == http.StatusUnauthorized {
		t.Fatal("a request that matched no frozen route must not become 401")
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
		authentication.Rejected("should not run"),
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
		authentication.Accepted(Principal{Subject: "u-1", AuthMethod: "jwt"}),
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
		authentication.Rejected("unused"),
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
		authentication.Rejected("unused"),
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

func TestAuthenticationMiddlewareOrderIsAuthPhase(t *testing.T) {
	t.Parallel()

	var middleware Middleware = &authenticationMiddleware{}
	if got := middleware.Order(); got.Phase != PhaseAuth {
		t.Fatalf("Order().Phase = %v, want %v", got.Phase, PhaseAuth)
	}
}
