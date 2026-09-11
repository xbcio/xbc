package web

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
)

func mustPolicySet(t *testing.T, cfg SecurityConfig) *policySet {
	t.Helper()
	set, err := newPolicySet(cfg.normalize())
	if err != nil {
		t.Fatalf("newPolicySet() error = %v", err)
	}
	return set
}

func TestPolicySetApplicationRuleOverridesRouteDeclaration(t *testing.T) {
	t.Parallel()

	publicPolicy := Public()
	route := RouteInfo{Method: http.MethodGet, Path: "/metrics", Auth: &publicPolicy}

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{{
			Match:        "GET /metrics",
			Authenticate: []authentication.Scheme{"jwt"},
		}},
	})

	got := set.resolve(route)
	if got.permit {
		t.Fatal("application rule must be able to tighten a route declared public")
	}
	if got.tier != tierApplicationRule {
		t.Fatalf("tier = %s, want %s", got.tier, tierApplicationRule)
	}
	if schemes := got.selection.Schemes(); !reflect.DeepEqual(schemes, []authentication.Scheme{"jwt"}) {
		t.Fatalf("selection = %v, want [jwt]", schemes)
	}
}

func TestPolicySetFallsBackToRouteDeclaration(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{})

	t.Run("route declared public stays public under default deny", func(t *testing.T) {
		t.Parallel()
		publicPolicy := Public()
		route := RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &publicPolicy}
		got := set.resolve(route)
		if !got.permit {
			t.Fatal("default deny must not override a route-level public declaration")
		}
		if got.tier != tierRoute {
			t.Fatalf("tier = %s, want %s", got.tier, tierRoute)
		}
	})

	t.Run("route declared schemes are honored", func(t *testing.T) {
		t.Parallel()
		accepts := Accepts("session")
		route := RouteInfo{Method: http.MethodGet, Path: "/ui", Auth: &accepts}
		got := set.resolve(route)
		if got.permit {
			t.Fatal("route with explicit schemes must not be permitted")
		}
		if got.tier != tierRoute {
			t.Fatalf("tier = %s, want %s", got.tier, tierRoute)
		}
		if schemes := got.selection.Schemes(); !reflect.DeepEqual(schemes, []authentication.Scheme{"session"}) {
			t.Fatalf("selection = %v, want [session]", schemes)
		}
	})
}

func TestPolicySetDefaultTierAppliesOnlyWhenUncovered(t *testing.T) {
	t.Parallel()

	route := RouteInfo{Method: http.MethodGet, Path: "/orders"}

	t.Run("deny uses the manager restrictive default selection", func(t *testing.T) {
		t.Parallel()
		set := mustPolicySet(t, SecurityConfig{Default: SecurityDeny})
		got := set.resolve(route)
		if got.permit {
			t.Fatal("default deny must not permit an uncovered route")
		}
		if got.tier != tierDefault {
			t.Fatalf("tier = %s, want %s", got.tier, tierDefault)
		}
		if !got.selection.UsesDefault() {
			t.Fatal("default deny must resolve to the manager restrictive default selection")
		}
	})

	t.Run("permit lets an uncovered route through", func(t *testing.T) {
		t.Parallel()
		set := mustPolicySet(t, SecurityConfig{Default: SecurityPermit})
		got := set.resolve(route)
		if !got.permit {
			t.Fatal("default permit must permit an uncovered route")
		}
		if got.tier != tierDefault {
			t.Fatalf("tier = %s, want %s", got.tier, tierDefault)
		}
	})
}

func TestPolicySetFirstMatchingRuleWins(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{
			{Match: "GET /api/v1/health", Permit: true},
			{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
		},
	})

	health := set.resolve(RouteInfo{Method: http.MethodGet, Path: "/api/v1/health"})
	if !health.permit || health.ruleIndex != 0 {
		t.Fatalf("health resolved to permit=%v rule=%d, want true/0", health.permit, health.ruleIndex)
	}

	orders := set.resolve(RouteInfo{Method: http.MethodGet, Path: "/api/v1/orders"})
	if orders.permit || orders.ruleIndex != 1 {
		t.Fatalf("orders resolved to permit=%v rule=%d, want false/1", orders.permit, orders.ruleIndex)
	}
}

func TestPolicySetCompileIndexesEveryFrozenRoute(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}}},
	})
	routes := []RouteInfo{
		{Method: http.MethodGet, Path: "/api/v1/orders"},
		{Method: http.MethodPost, Path: "/api/v1/orders"},
		{Method: http.MethodGet, Path: "/other"},
	}

	compiled := set.compile(routes)
	if len(compiled) != len(routes) {
		t.Fatalf("compile() produced %d entries, want %d", len(compiled), len(routes))
	}
	for _, route := range routes {
		if _, ok := compiled[routeKey(route.Method, route.Path)]; !ok {
			t.Fatalf("compile() missing entry for %s %s", route.Method, route.Path)
		}
	}
	if got := compiled[routeKey(http.MethodGet, "/other")]; got.tier != tierDefault {
		t.Fatalf("uncovered route tier = %s, want %s", got.tier, tierDefault)
	}
}

func TestPolicySetRejectsUnreachableRule(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{
			{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
			{Match: "/api/v1/health", Permit: true},
		},
	})

	err := set.validateReachability()
	if err == nil {
		t.Fatal("validateReachability() error = nil, want unreachable rule error")
	}
	for _, want := range []string{"policy 1", "/api/v1/health", "policy 0", "/api/**"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
}

func TestPolicySetAcceptsSpecificRuleBeforeBroadRule(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{
			{Match: "/api/v1/health", Permit: true},
			{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
		},
	})

	if err := set.validateReachability(); err != nil {
		t.Fatalf("validateReachability() error = %v, want nil", err)
	}
}

func TestPolicySetRejectsUnreferencedRegisteredScheme(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Default: SecurityPermit,
		Policies: []PolicyRule{
			{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
		},
	})

	err := set.validateSchemeCoverage([]authentication.Scheme{"jwt", "apikey"})
	if err == nil {
		t.Fatal("validateSchemeCoverage() error = nil, want unreferenced scheme error")
	}
	if !strings.Contains(err.Error(), `"apikey"`) {
		t.Fatalf("error = %q, want substring \"apikey\"", err)
	}

	if err := set.validateSchemeCoverage([]authentication.Scheme{"jwt"}); err != nil {
		t.Fatalf("validateSchemeCoverage(jwt) error = %v, want nil", err)
	}
}

func TestPolicySetDefaultDenyTreatsEverySchemeAsReferenced(t *testing.T) {
	t.Parallel()

	// Under default deny, uncovered routes resolve through the manager's
	// restrictive default selection, which can reach any registered scheme.
	// Reporting those schemes as unreferenced would be a false alarm.
	set := mustPolicySet(t, SecurityConfig{Default: SecurityDeny})
	if err := set.validateSchemeCoverage([]authentication.Scheme{"jwt", "apikey"}); err != nil {
		t.Fatalf("validateSchemeCoverage() error = %v, want nil under default deny", err)
	}
}
