package web

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/plugin/ordering"
)

// renderMiddlewareChain renders the ordered middleware section, one line per
// entry, with soft-order hints (after=/before=) appended when declared. It
// is the "web: " counterpart of the pre-split kernel's renderMiddlewareChain
// -- same layout, moved here because middleware ordering is now something
// only web owns (package-layout design §4: "startup report stays in the module that owns the data")
func renderMiddlewareChain(chain []mwEntry) string {
	nameWidth := 0
	for _, e := range chain {
		if l := len(e.qname); l > nameWidth {
			nameWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: middleware chain (%d)", len(chain))
	for i, e := range chain {
		line := fmt.Sprintf("  %d. %-*s  [%s]", i+1, nameWidth, e.qname, e.Phase.String())
		if len(e.After) > 0 {
			line += "  after=" + strings.Join(e.After, ",")
		}
		if len(e.Before) > 0 {
			line += "  before=" + strings.Join(e.Before, ",")
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// renderRouteTable renders the frozen route table, one line per route,
// columns aligned to this run's actual content width -- hardcoding a width
// would misalign the moment a route's method or path is longer or shorter
// than whatever the author happened to test with.
func renderRouteTable(routes []RouteInfo) string {
	methodWidth := 0
	for _, r := range routes {
		if l := len(r.Method); l > methodWidth {
			methodWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: route table (%d)", len(routes))
	for i, r := range routes {
		fmt.Fprintf(&b, "\n  %d. %-*s  %s", i+1, methodWidth, r.Method, r.Path)
	}
	return b.String()
}

// renderSoftMisses renders every soft ordering constraint that pointed at a
// middleware which does not exist -- harmless to startup, but silent typos
// in After/Before are exactly the kind of bug nobody notices without this
// line.
func renderSoftMisses(misses []ordering.Miss) string {
	var b strings.Builder
	b.WriteString("web: unmatched soft constraints (startup unaffected)")
	for _, m := range misses {
		dir := "After"
		if m.Dir == ordering.Before {
			dir = "Before"
		}
		fmt.Fprintf(&b, "\n  %s.%s = %q — no such middleware, ignored\n    → spelling error? or forgot to enable the corresponding plugin?", m.Node, dir, m.Ref)
	}
	return b.String()
}
