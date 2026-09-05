package web

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
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

func renderSoftMisses(misses []MiddlewareOrderMiss) string {
	var b strings.Builder
	b.WriteString("web: unmatched soft constraints (startup unaffected)")
	for _, miss := range misses {
		direction := "After"
		if miss.Direction == ordering.Before {
			direction = "Before"
		}
		fmt.Fprintf(
			&b,
			"\n  %s.%s = %q — no matching middleware, ignored\n    → spelling error? or forgot to enable the corresponding plugin?",
			miss.Middleware,
			direction,
			miss.Reference,
		)
	}
	return b.String()
}
