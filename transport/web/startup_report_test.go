package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
	})

	for _, want := range []string{"/metrics", "application-rule", "/orders", "default"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report = %q, want substring %q", got, want)
		}
	}
	if strings.Contains(got, "rule -1") {
		t.Fatal("report must not print a rule number for a decision no rule made")
	}
}
