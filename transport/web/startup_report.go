package web

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

// renderMiddlewareChain renders the frozen middleware chain using producer
// identities. Identity is the complete middleware name in the plugin model.
func renderMiddlewareChain(chain []plugin.Entry[Middleware]) string {
	nameWidth := 0
	orders := make([]Order, len(chain))
	for i, entry := range chain {
		orders[i] = entry.Value.Order()
		if l := len(entry.Identity.String()); l > nameWidth {
			nameWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: middleware chain (%d)", len(chain))
	for i, entry := range chain {
		order := orders[i]
		line := fmt.Sprintf("  %d. %-*s  [%s]", i+1, nameWidth, entry.Identity, order.Phase)
		if len(order.After) > 0 {
			line += "  after=" + joinOrderRefs(order.After)
		}
		if len(order.Before) > 0 {
			line += "  before=" + joinOrderRefs(order.Before)
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

func joinOrderRefs(refs []OrderRef) string {
	values := make([]string, len(refs))
	for i, ref := range refs {
		values[i] = ref.String()
	}
	return strings.Join(values, ",")
}

func renderRouteTable(routes []RouteInfo) string {
	methodWidth := 0
	for _, route := range routes {
		if l := len(route.Method); l > methodWidth {
			methodWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: route table (%d)", len(routes))
	for i, route := range routes {
		fmt.Fprintf(&b, "\n  %d. %-*s  %s", i+1, methodWidth, route.Method, route.Path)
	}
	return b.String()
}

// renderPublicEndpoints lists every route that resolved to permit and reports
// the count. When defaultPermit is true the global fallback is fail-open,
// which deserves a prominent warning because every uncovered route is public.
func renderPublicEndpoints(routes []RouteInfo, defaultPermit bool) string {
	methodWidth := 0
	for _, route := range routes {
		if l := len(route.Method); l > methodWidth {
			methodWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: public endpoints (%d)", len(routes))
	if defaultPermit {
		b.WriteString("\n  WARNING: default policy is permit — every endpoint not covered by a rule or route declaration is public")
	}
	for i, route := range routes {
		fmt.Fprintf(&b, "\n  %d. %-*s  %s", i+1, methodWidth, route.Method, route.Path)
	}
	return b.String()
}

// renderPolicyDecisions renders the per-route effective policy table so an
// operator can see which tier decided each route's authentication requirement.
func renderPolicyDecisions(decisions []policyDecision) string {
	methodWidth := 0
	pathWidth := 0
	for _, d := range decisions {
		if l := len(d.route.Method); l > methodWidth {
			methodWidth = l
		}
		if l := len(d.route.Path); l > pathWidth {
			pathWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: policy decisions (%d)", len(decisions))
	for i, d := range decisions {
		outcome := "deny"
		if d.policy.permit {
			outcome = "permit"
		} else if schemes := d.policy.selection.Schemes(); len(schemes) > 0 {
			parts := make([]string, len(schemes))
			for j, s := range schemes {
				parts[j] = string(s)
			}
			outcome = strings.Join(parts, ",")
		}
		tier := fmt.Sprintf("(%s", d.policy.tier)
		if d.policy.ruleIndex >= 0 {
			tier += fmt.Sprintf(", rule %d", d.policy.ruleIndex)
		}
		tier += ")"
		fmt.Fprintf(&b, "\n  %d. %-*s  %-*s  -> %-8s  %s", i+1, methodWidth, d.route.Method, pathWidth, d.route.Path, outcome, tier)
	}
	return b.String()
}

// renderSoftMisses reports Prefer references that had no target in the frozen
// chain. This is what a soft reference is documented to do -- a plugin declares
// "order me relative to X if X is here", and X is not -- so the report states
// the fact rather than suggesting a fault. The reference is a typed key in the
// declaring plugin's own Go source, so the operator reading this log never
// wrote it; the only thing they can act on is whether the named plugin was
// meant to be part of this application at all.
func renderSoftMisses(misses []MiddlewareOrderMiss) string {
	var b strings.Builder
	b.WriteString("web: optional ordering preferences with no target (chain unaffected)")
	for _, miss := range misses {
		fmt.Fprintf(
			&b,
			"\n  %s prefers to run %s %s, which contributes no middleware\n    → %s is not selected in this build, or not enabled by configuration",
			miss.Middleware,
			miss.Direction,
			miss.Reference,
			miss.Reference,
		)
	}
	return b.String()
}
