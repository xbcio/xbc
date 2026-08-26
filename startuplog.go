package xbc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/log"
)

// printStartupLog renders spec §4.4's four sections: the assembled plugin
// table, the middleware chain, unresolved soft constraints, and (when
// migration did not run) a reminder of how to run it. A narrow-interface
// framework silently skips a plugin whose method name is misspelled -- the
// only backstop against that is spelling out exactly what got wired up.
func (a *App) printStartupLog(order []*instance) {
	l := log.L()
	l.Info(buildPluginTable(a, order))
	if len(a.middlewareChain) > 0 {
		l.Info(renderMiddlewareChain(a.middlewareChain))
	}
	if len(a.softMisses) > 0 {
		l.Info(renderSoftMisses(a.softMisses))
	}
	if note := renderMigrationNotice(a, order); note != "" {
		l.Info(note)
	}
}

// capabilityTokens lists a plugin instance's optional-interface capabilities
// in the fixed order laid out by spec §5.1's interface table: config, init,
// migrate, middleware, routes(N), runner, health, stop. (Spec §4.4's own
// example swaps routes(N) and migrate for exactly one row -- "user" -- while
// every other row in that same example follows this order; that swap is
// treated as a documentation slip, not a rule to reproduce.)
func capabilityTokens(a *App, inst *instance) []string {
	var tokens []string
	if _, ok := inst.plugin.(Configurable); ok {
		tokens = append(tokens, "config")
	}
	if _, ok := inst.plugin.(Initializer); ok {
		tokens = append(tokens, "init")
	}
	if _, ok := inst.plugin.(Migrator); ok {
		tokens = append(tokens, "migrate")
	}
	if _, ok := inst.plugin.(MiddlewareProvider); ok {
		tokens = append(tokens, "middleware")
	}
	if _, ok := inst.plugin.(RouteProvider); ok {
		tokens = append(tokens, fmt.Sprintf("routes(%d)", a.routeCounts[inst.id()]))
	}
	if _, ok := inst.plugin.(Runner); ok {
		tokens = append(tokens, "runner")
	}
	if _, ok := inst.plugin.(HealthChecker); ok {
		tokens = append(tokens, "health")
	}
	if _, ok := inst.plugin.(Closer); ok {
		tokens = append(tokens, "stop")
	}
	return tokens
}

// depsAndProvidesSuffix renders an instance's hard type dependencies and
// declared products for the trailing column of the plugin table.
func depsAndProvidesSuffix(inst *instance) string {
	var parts []string
	if len(inst.deps.Types) > 0 {
		var ts []string
		for _, d := range inst.deps.Types {
			ts = append(ts, d.String())
		}
		parts = append(parts, "requires "+strings.Join(ts, " "))
	}
	if len(inst.provides) > 0 {
		var ts []string
		for _, d := range inst.provides {
			ts = append(ts, d.String())
		}
		parts = append(parts, "provides "+strings.Join(ts, " "))
	}
	return strings.Join(parts, "  ")
}

// fmtSprintfRow renders one plugin-table row with the given column widths.
// This is buildPluginTable's own implementation detail -- unlike an earlier
// draft, no test calls this directly to construct its expected value.
// Sharing a formatter between production code and a test's "want" string
// would make the row layout itself unpinned: reformat the columns however
// you like (change the separator width, flip left/right alignment) and both
// sides drift together, so the test would stay green no matter what the
// actual on-screen layout became. buildPluginTable's tests instead spell
// their expected rows out as independent literal strings.
func fmtSprintfRow(labelWidth int, label string, capsWidth int, caps, suffix string) string {
	return strings.TrimRight(fmt.Sprintf("  %-*s  %-*s  %s", labelWidth, label, capsWidth, caps, suffix), " ")
}

// buildPluginTable renders the "assembled" section: one row per instance,
// columns aligned to the actual content width of this run -- hardcoding a
// width would misalign the moment a plugin name or capability list is
// longer or shorter than whatever the author happened to test with.
func buildPluginTable(a *App, order []*instance) string {
	type row struct {
		label    string
		caps     string
		suffix   string
		explicit bool
	}
	rows := make([]row, len(order))
	labelWidth, capsWidth := 0, 0
	for i, inst := range order {
		r := row{
			label:    inst.label(),
			caps:     strings.Join(capabilityTokens(a, inst), " "),
			suffix:   depsAndProvidesSuffix(inst),
			explicit: inst.src == sourceRegister,
		}
		rows[i] = r
		if len(r.label) > labelWidth {
			labelWidth = len(r.label)
		}
		if len(r.caps) > capsWidth {
			capsWidth = len(r.caps)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "xbc: 装配完成，%d 个插件实例", len(order))
	for _, r := range rows {
		b.WriteString("\n")
		b.WriteString(fmtSprintfRow(labelWidth, r.label, capsWidth, r.caps, r.suffix))
		if r.explicit {
			b.WriteString("\n")
			b.WriteString(strings.Repeat(" ", 2+labelWidth+2+capsWidth+2))
			b.WriteString("↑ 显式 Register")
		}
	}
	return b.String()
}

// renderMiddlewareChain renders the ordered middleware section, one line per
// entry, with soft-order hints (after=/before=) appended when declared.
func renderMiddlewareChain(chain []mwEntry) string {
	nameWidth := 0
	for _, e := range chain {
		if l := len(e.qname); l > nameWidth {
			nameWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: 中间件链（%d）", len(chain))
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

// renderSoftMisses renders every soft ordering constraint that pointed at a
// plugin which does not exist -- harmless to startup, but silent typos in
// After/Before are exactly the kind of bug nobody notices without this line.
func renderSoftMisses(misses []graph.Miss) string {
	var b strings.Builder
	b.WriteString("xbc: 软约束未命中（不影响启动）")
	for _, m := range misses {
		dir := "After"
		if m.Dir == "before" {
			dir = "Before"
		}
		fmt.Fprintf(&b, "\n  %s.%s = %q —— 无此插件，忽略\n    → 拼写错误？还是忘了启用 plugins.%s？", m.Node, dir, m.Ref, m.Ref)
	}
	return b.String()
}

// renderMigrationNotice reports how many instances declared Migrate but had
// it skipped this run. Spec §4.4's example counts "12 个模型" -- a concept
// gorm itself owns (it knows how many models it registered); the kernel does
// not know what a "model" is. The count here is the number of Migrator-
// implementing instances instead, which is the same warning ("something
// declared it needs migrating and didn't get it") stated in a vocabulary the
// kernel actually has.
func renderMigrationNotice(a *App, order []*instance) string {
	if a.migrate {
		return ""
	}
	n := 0
	for _, inst := range order {
		if _, ok := inst.plugin.(Migrator); ok {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("xbc: 迁移未执行（%d 个插件声明了 Migrate，待检查）\n  → 需要迁移请使用 ./myapp migrate 或 --migrate", n)
}
