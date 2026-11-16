package web

import (
	"net/http"
	"reflect"
	"strconv"
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

	err := set.validateSchemeCoverage([]authentication.Scheme{"jwt", "apikey"}, nil, nil)
	if err == nil {
		t.Fatal("validateSchemeCoverage() error = nil, want unreferenced scheme error")
	}
	if !strings.Contains(err.Error(), `"apikey"`) {
		t.Fatalf("error = %q, want substring \"apikey\"", err)
	}

	if err := set.validateSchemeCoverage([]authentication.Scheme{"jwt"}, nil, nil); err != nil {
		t.Fatalf("validateSchemeCoverage(jwt) error = %v, want nil", err)
	}
}

func TestPolicySetDefaultDenyTreatsEverySchemeAsReferenced(t *testing.T) {
	t.Parallel()

	// Under default deny, uncovered routes resolve through the manager's
	// restrictive default selection, which can reach any registered scheme.
	// Reporting those schemes as unreferenced would be a false alarm.
	set := mustPolicySet(t, SecurityConfig{Default: SecurityDeny})
	if err := set.validateSchemeCoverage([]authentication.Scheme{"jwt", "apikey"}, nil, nil); err != nil {
		t.Fatalf("validateSchemeCoverage() error = %v, want nil under default deny", err)
	}
}

func TestPolicySetCountsRouteDeclarationsAsSchemeReferences(t *testing.T) {
	t.Parallel()

	// A route-level .Auth() declaration is tier 2 and never enters the rule
	// list, so reachability has to read the compiled decision instead.
	set := mustPolicySet(t, SecurityConfig{Default: SecurityPermit})
	jwt := Accepts("jwt")
	routes := []RouteInfo{{Method: http.MethodGet, Path: "/orders", Auth: &jwt}}
	compiled := set.compile(routes)

	if err := set.validateSchemeCoverage([]authentication.Scheme{"jwt"}, routes, compiled); err != nil {
		t.Fatalf("validateSchemeCoverage() error = %v, want nil: the route declaration selects jwt", err)
	}

	// The same table with an additional registered scheme still fails, so the
	// widened definition is not "any registered scheme is fine".
	err := set.validateSchemeCoverage([]authentication.Scheme{"jwt", "apikey"}, routes, compiled)
	if err == nil {
		t.Fatal("validateSchemeCoverage() error = nil, want the genuinely unreferenced scheme to fail")
	}
	if !strings.Contains(err.Error(), `"apikey"`) {
		t.Fatalf("validateSchemeCoverage() error = %q, want it to name scheme \"apikey\"", err)
	}
}

func TestPolicySetTierOneRuleHidesRouteDeclarationFromCoverage(t *testing.T) {
	t.Parallel()

	// Tier 1 overrides tier 2: the permit rule decides /api/orders, so the
	// route's own Accepts("jwt") is not in effect and must not keep jwt alive.
	set := mustPolicySet(t, SecurityConfig{
		Default:  SecurityPermit,
		Policies: []PolicyRule{{Match: "/api/**", Permit: true}},
	})
	jwt := Accepts("jwt")
	routes := []RouteInfo{{Method: http.MethodGet, Path: "/api/orders", Auth: &jwt}}
	compiled := set.compile(routes)

	err := set.validateSchemeCoverage([]authentication.Scheme{"jwt"}, routes, compiled)
	if err == nil {
		t.Fatal("validateSchemeCoverage() error = nil, want the overridden declaration to leave jwt unreachable")
	}
	if !strings.Contains(err.Error(), `"jwt"`) {
		t.Fatalf("validateSchemeCoverage() error = %q, want it to name scheme \"jwt\"", err)
	}
}

// TestPolicySetCoverage pins the mandatory-rule check and the facts that make a
// mounted subtree usable: matching is done against frozen RouteInfo rows, so a
// mounted row counts at its prefix -- the only row that exists for the subtree
// -- and a path inside that prefix is not a row at all.
func TestPolicySetCoverage(t *testing.T) {
	t.Parallel()

	routes := []RouteInfo{
		{Method: http.MethodGet, Path: "/health"},
		{Method: http.MethodGet, Path: "/assets", Mounted: true},
		{Method: http.MethodPost, Path: "/assets/upload"},
	}

	tests := []struct {
		name     string
		rule     PolicyRule
		wantHits int
		wantErr  bool
	}{
		{
			name:     "exact route",
			rule:     PolicyRule{Match: "GET /health", Permit: true},
			wantHits: 1,
		},
		{
			name:     "wildcard route",
			rule:     PolicyRule{Match: "/assets/**", Authenticate: []authentication.Scheme{"jwt"}},
			wantHits: 2,
		},
		{
			name:     "mount row matches at its prefix",
			rule:     PolicyRule{Match: "GET /assets", Permit: true},
			wantHits: 1,
		},
		{
			name:     "path inside a mounted subtree is not a row",
			rule:     PolicyRule{Match: "GET /assets/logo.png", Permit: true},
			wantHits: 0,
			wantErr:  true,
		},
		{
			name:    "mandatory rule matching nothing fails",
			rule:    PolicyRule{Match: "GET /missing", Permit: true},
			wantErr: true,
		},
		{
			name:     "optional rule may match nothing",
			rule:     PolicyRule{Match: "GET /missing", Permit: true, Optional: true},
			wantHits: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			set := mustPolicySet(t, SecurityConfig{Policies: []PolicyRule{tt.rule}})
			coverages := set.coverage(routes)
			if len(coverages) != 1 {
				t.Fatalf("coverage() returned %d entries, want 1", len(coverages))
			}
			if coverages[0].hits != tt.wantHits {
				t.Fatalf("hits = %d, want %d", coverages[0].hits, tt.wantHits)
			}

			err := set.validateCoverage(coverages)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateCoverage() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateCoverage() error = nil, want the unmatched rule to fail")
			}
			for _, want := range []string{"policy 0", strconv.Quote(tt.rule.Match), "optional: true"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want substring %q", err, want)
				}
			}
		})
	}
}

// TestPolicyResolverFirstMatchWins pins that the exported resolver preserves
// the rule order the runtime enforces: when two rules match, the decision must
// name the first one, not whichever rule happens to be more specific.
func TestPolicyResolverFirstMatchWins(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{
			{Match: "GET /api/orders", Permit: true},
			{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
		},
	})
	resolver := PolicyResolver{policies: set, defaultSchemes: []authentication.Scheme{"jwt"}}

	got := resolver.Resolve(RouteInfo{Method: http.MethodGet, Path: "/api/orders"})
	if got.Outcome != PolicyPermit {
		t.Fatalf("outcome = %s, want permit from the first matching rule", got.Outcome)
	}
	if got.Source != PolicySourceRule || got.RuleIndex != 0 || got.RuleMatch != "GET /api/orders" {
		t.Fatalf("decision = %+v, want rule 0 to be named", got)
	}

	got = resolver.Resolve(RouteInfo{Method: http.MethodPost, Path: "/api/orders"})
	if got.Outcome != PolicyAuthenticate || got.Source != PolicySourceRule || got.RuleIndex != 1 {
		t.Fatalf("decision = %+v, want the second rule for the unmatched method", got)
	}
}

// TestPolicyResolverManagementRouteIsNotApplicable pins that the read-only API
// does not invent a serving-plane policy for a row whose chain runs no
// authentication middleware.
func TestPolicyResolverManagementRouteIsNotApplicable(t *testing.T) {
	t.Parallel()

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{{Match: "/**", Permit: true}},
	})
	resolver := PolicyResolver{policies: set}

	got := resolver.Resolve(RouteInfo{Method: http.MethodGet, Path: "/-/metrics", Management: true})
	if got.Outcome != PolicyNotApplicable {
		t.Fatalf("outcome = %s, want %s", got.Outcome, PolicyNotApplicable)
	}
	if len(got.Schemes) != 0 || got.RuleIndex != -1 || got.RuleMatch != "" {
		t.Fatalf("decision = %+v, want no schemes, no rule, and index -1", got)
	}
}

// TestPolicyResolverZeroValueIsFailClosed pins the documented zero value: a
// resolver a caller never received from the framework must not resolve routes
// as public.
func TestPolicyResolverZeroValueIsFailClosed(t *testing.T) {
	t.Parallel()

	got := PolicyResolver{}.Resolve(RouteInfo{Method: http.MethodGet, Path: "/orders"})
	if got.Outcome != PolicyDeny {
		t.Fatalf("outcome = %s, want deny", got.Outcome)
	}
	if got.Source != PolicySourceDefault || got.RuleIndex != -1 {
		t.Fatalf("decision = %+v, want the default tier and no rule", got)
	}
	if len(got.Schemes) != 0 {
		t.Fatalf("schemes = %v, want none", got.Schemes)
	}
}
