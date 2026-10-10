package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

const rateLimitKey plugin.Key = "ratelimit"

// TestRenderSoftMissesDoesNotBlameTheOperator pins the audience of this report.
// An OrderRef is built from a typed key in the declaring plugin's own Go source
// -- the inventory guard rejects string literals here -- so the operator reading
// the startup log never wrote the reference and cannot have misspelled it. The
// report must name the two causes they can actually check instead of asking
// them to look for a typo.
func TestRenderSoftMissesDoesNotBlameTheOperator(t *testing.T) {
	t.Parallel()

	report := renderSoftMisses([]MiddlewareOrderMiss{{
		Middleware: plugin.Identity{Plugin: securityKey},
		Reference:  Prefer(rateLimitKey),
		Direction:  ordering.Before,
	}})

	assert.NotContains(t, report, "spelling")
	assert.Contains(t, report, securityKey.String())
	assert.Contains(t, report, rateLimitKey.String())
	assert.Contains(t, report, "not selected in this build")
	assert.Contains(t, report, "not enabled by configuration")
}

// TestRenderSoftMissesReportsTheDeclaredDirection guards a renderer that
// hard-codes one direction: "runs before X" and "runs after X" are opposite
// operational facts, and the earlier hand-rolled switch reported every
// non-Before value as After, including the invalid zero.
func TestRenderSoftMissesReportsTheDeclaredDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		direction ordering.Direction
		want      string
	}{
		{name: "before", direction: ordering.Before, want: "run before ratelimit"},
		{name: "after", direction: ordering.After, want: "run after ratelimit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := renderSoftMisses([]MiddlewareOrderMiss{{
				Middleware: plugin.Identity{Plugin: securityKey},
				Reference:  Prefer(rateLimitKey),
				Direction:  tt.direction,
			}})

			assert.Contains(t, report, tt.want)
		})
	}
}

// TestRenderSoftMissesReportsEveryMiss guards the loop: a plugin may declare
// several preferences, and reporting only the first would hide the rest behind
// a report that looks complete.
func TestRenderSoftMissesReportsEveryMiss(t *testing.T) {
	t.Parallel()

	report := renderSoftMisses([]MiddlewareOrderMiss{
		{
			Middleware: plugin.Identity{Plugin: securityKey},
			Reference:  Prefer(tracingKey),
			Direction:  ordering.After,
		},
		{
			Middleware: plugin.Identity{Plugin: securityKey},
			Reference:  PreferInstance(metricsKey, "regional"),
			Direction:  ordering.Before,
		},
	})

	assert.Contains(t, report, "run after tracing")
	assert.Contains(t, report, "run before metrics[regional]")
}

func TestRenderPublicEndpointsWarnsOnDefaultPermit(t *testing.T) {
	t.Parallel()

	got := renderPublicEndpoints([]RouteInfo{
		{Method: http.MethodGet, Path: "/healthz"},
	}, true)

	if !strings.Contains(got, "/healthz") {
		t.Fatalf("report = %q, want it to list /healthz", got)
	}
	if !strings.Contains(strings.ToLower(got), "permit") {
		t.Fatalf("report = %q, want a default-permit warning", got)
	}
}

func TestRenderPolicyDecisionsNamesTheDecidingTier(t *testing.T) {
	t.Parallel()

	got := renderPolicyDecisions([]policyDecision{
		{
			route:  RouteInfo{Method: http.MethodGet, Path: "/metrics"},
			policy: effectivePolicy{tier: tierApplicationRule, ruleIndex: 2},
		},
		{
			route:  RouteInfo{Method: http.MethodGet, Path: "/orders"},
			policy: effectivePolicy{tier: tierDefault, ruleIndex: -1},
		},
	}, []authentication.Scheme{"jwt"})

	for _, want := range []string{"/metrics", "application-rule", "/orders", "default"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report = %q, want substring %q", got, want)
		}
	}
	if strings.Contains(got, "rule -1") {
		t.Fatal("report must not print a rule number for a decision no rule made")
	}
}

// TestRenderPolicyDecisionsLabelsDefaultTierAuthenticationNotDeny pins the fix
// for a route that authenticates through the manager's default selection
// (tierDefault, permit false): Selection.Schemes() returns nil for a default
// selection by design, so without defaultSchemes the row previously fell
// through to the literal "deny" even though the route actually authenticates.
func TestRenderPolicyDecisionsLabelsDefaultTierAuthenticationNotDeny(t *testing.T) {
	t.Parallel()

	got := renderPolicyDecisions([]policyDecision{
		{
			route: RouteInfo{Method: http.MethodGet, Path: "/orders"},
			policy: effectivePolicy{
				tier:      tierDefault,
				ruleIndex: -1,
				selection: authentication.DefaultSelection(),
			},
		},
	}, []authentication.Scheme{"jwt", "apikey"})

	if !strings.Contains(got, "jwt") || !strings.Contains(got, "apikey") {
		t.Fatalf("report = %q, want it to name the default schemes jwt and apikey", got)
	}
	if strings.Contains(got, "-> deny") {
		t.Fatalf("report = %q, must not label a default-tier authenticated route \"deny\"", got)
	}
}

// TestMountedRowsAreMarkedInEveryRouteReport pins that a Router.Mount's rows
// are recognisable as a subtree wherever the startup report lists routes. A
// mount writes one row per method at its prefix -- the same rows an Any
// registration writes -- while the declarations that can cover it are written
// against the prefix, so an operator who cannot tell the two apart is reading
// a report that hides the mounted surface. The unmarked half matters just as
// much: a mark on every row would carry no information.
func TestMountedRowsAreMarkedInEveryRouteReport(t *testing.T) {
	t.Parallel()

	mounted := RouteInfo{Method: http.MethodGet, Path: "/flow", Mounted: true}
	plain := RouteInfo{Method: http.MethodGet, Path: "/orders"}
	decision := func(route RouteInfo) policyDecision {
		return policyDecision{
			route:  route,
			policy: effectivePolicy{tier: tierDefault, ruleIndex: -1, selection: authentication.DefaultSelection()},
		}
	}

	reports := map[string]string{
		"route table":      renderRouteTable([]RouteInfo{mounted}),
		"public endpoints": renderPublicEndpoints([]RouteInfo{mounted}, false),
		"policy decisions": renderPolicyDecisions([]policyDecision{decision(mounted)}, nil),
	}
	for name, report := range reports {
		if !strings.Contains(report, "(mounted subtree)") {
			t.Fatalf("%s = %q, want the mounted row marked as a subtree", name, report)
		}
	}
	if strings.Contains(renderRouteTable([]RouteInfo{plain}), "(mounted subtree)") {
		t.Fatal("a route that covers a single path must not be marked as a mounted subtree")
	}
}

// TestUnmeteredRowsAreMarkedInTheRouteTable pins that an exemption from the
// in-flight gate is visible where routes are listed. It is the one property of
// a route the table would otherwise hide: everything else about the row -- the
// method, the path, the mount mark -- reads the same as a metered route's, and
// the exemption is a deliberate hole in the process's load shedder, so an
// operator reading the startup report or the validate output has to be able to
// see which paths are answered from a saturated process.
func TestUnmeteredRowsAreMarkedInTheRouteTable(t *testing.T) {
	t.Parallel()

	unmetered := RouteInfo{Method: http.MethodGet, Path: "/healthz", Unmetered: true}
	plain := RouteInfo{Method: http.MethodGet, Path: "/orders"}

	report := renderRouteTable([]RouteInfo{unmetered})
	if !strings.Contains(report, "(unmetered)") {
		t.Fatalf("route table = %q, want the exempt row marked", report)
	}
	if strings.Contains(renderRouteTable([]RouteInfo{plain}), "(unmetered)") {
		t.Fatal("a metered route must not be marked as unmetered")
	}
}
