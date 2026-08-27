// report.go
package web

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/topology"
)

// renderMiddlewareChain renders the ordered middleware section, one line per
// entry, with soft-order hints (after=/before=) appended when declared. It
// is the "web: " counterpart of the pre-split kernel's renderMiddlewareChain
// -- same layout, moved here because middleware ordering is now something
// only web owns (package-layout design §4: "启动报告留在拥有数据的模块").
func renderMiddlewareChain(chain []mwEntry) string {
	nameWidth := 0
	for _, e := range chain {
		if l := len(e.qname); l > nameWidth {
			nameWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web: 中间件链（%d）", len(chain))
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
	fmt.Fprintf(&b, "web: 路由表（%d）", len(routes))
	for i, r := range routes {
		fmt.Fprintf(&b, "\n  %d. %-*s  %s", i+1, methodWidth, r.Method, r.Path)
	}
	return b.String()
}

// renderSoftMisses renders every soft ordering constraint that pointed at a
// middleware which does not exist -- harmless to startup, but silent typos
// in After/Before are exactly the kind of bug nobody notices without this
// line.
func renderSoftMisses(misses []topology.Miss) string {
	var b strings.Builder
	b.WriteString("web: 软约束未命中（不影响启动）")
	for _, m := range misses {
		dir := "After"
		if m.Dir == topology.Before {
			dir = "Before"
		}
		fmt.Fprintf(&b, "\n  %s.%s = %q —— 无此中间件，忽略\n    → 拼写错误？还是忘了启用对应插件？", m.Node, dir, m.Ref)
	}
	return b.String()
}
