package web

import (
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
