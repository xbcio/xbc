package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// This file holds the authentication-middleware tests that assert on the
// middleware's own resolved state -- the policy set it compiled, the tier each
// route landed in, whether RoutesReady accepted the table at all. None of them
// send a request, so none of them needs an engine; what they do need is
// newPolicySet, SecurityConfig.normalize, effectivePolicy's fields and the tier
// constants, which stay unexported. Everything that drives a real request lives
// in authentication_middleware_test.go (package web_test) on enginetest.

// internalCatalog builds a frozen catalog directly. routeCatalog is package
// private, so a test in this package can construct one without driving a whole
// Router.
func internalCatalog(routes []RouteInfo) RouteCatalog {
	index := make(map[string]RouteInfo, len(routes))
	for _, route := range routes {
		index[routeKey(route.Method, route.Path)] = route
	}
	return &routeCatalog{all: append([]RouteInfo(nil), routes...), index: index}
}

// internalAuth is an authenticator that only has to exist: these tests stop at
// RoutesReady, so Authenticate is never reached.
type internalAuth struct{ scheme authentication.Scheme }

func (a internalAuth) Scheme() authentication.Scheme { return a.scheme }

func (internalAuth) Authenticate(
	context.Context, authentication.Credential,
) (authentication.Result, error) {
	return authentication.Rejected("unused"), nil
}

// internalExtractor is the matching extractor stub, for the same reason.
type internalExtractor struct{ scheme authentication.Scheme }

func (e internalExtractor) Scheme() authentication.Scheme { return e.scheme }

func (internalExtractor) ExtractCredential(*Ctx) (authentication.CredentialResult, error) {
	return authentication.Absent(), nil
}

func TestAuthenticationMiddlewareRoutesReadyRejectsUnknownScheme(t *testing.T) {
	t.Parallel()

	manager, err := authentication.NewManager(authentication.ManagerOptions{
		Authenticators: []authentication.Authenticator{internalAuth{scheme: "jwt"}},
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

	err = middleware.RoutesReady(internalCatalog([]RouteInfo{
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

	err = middleware.RoutesReady(internalCatalog([]RouteInfo{
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

	if err := middleware.RoutesReady(internalCatalog([]RouteInfo{
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
	middleware, err := newAuthenticationMiddleware(
		SecurityConfig{Default: SecurityDeny},
		[]plugin.Entry[authentication.Authenticator]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: internalAuth{scheme: "jwt"}},
		},
		[]plugin.Entry[CredentialExtractor]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: internalExtractor{scheme: "jwt"}},
		},
	)
	if err != nil {
		t.Fatalf("newAuthenticationMiddleware() error = %v", err)
	}
	if err := middleware.RoutesReady(internalCatalog(routes)); err != nil {
		t.Fatalf("RoutesReady() error = %v", err)
	}

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
