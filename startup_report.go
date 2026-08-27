// startup_report.go renders and emits the startup report: what was actually
// assembled, what was declared but not enabled, which soft ordering
// constraints pointed at nothing, and whether migrations were skipped.
//
// This is not decoration. A framework built on optional interfaces silently
// ignores a plugin whose method name is misspelled -- Init versus init, Start
// with the wrong signature -- and there is no compiler diagnostic for "you
// meant to implement this interface and didn't". Printing the capabilities
// each instance was actually detected as having is the only backstop against
// that class of mistake, and it is why the table lists capabilities rather
// than just names.
//
// The rendering half and the emitting half used to be separate packages: the
// renderers lived in internal/startupreport, exported so this file could
// reach them. The stated reason was that a separate package let the renderers
// be tested "without constructing an App" -- but a package boundary is not
// what grants that. A test in this package can build a *container.Container
// directly and never call newApp, which is exactly what render_test.go does
// today. The boundary's only measurable effect was forcing two test fakes
// (bare, fakeHost) to be duplicated from fixtures that already existed
// elsewhere, because test helpers do not cross package boundaries.
//
// The responsibility split it expressed is real and survives as a rule inside
// this file: every render* function below returns a string and performs no
// I/O. Which logger the lines go to, and which of the four optional sections
// have anything to say this run, is report's job and report's alone.
package xbc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/internal/container"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/topology"
)

// report prints what was actually assembled: the instance table, anything
// that was declared but not enabled, every soft ordering constraint that
// pointed at nothing, and a reminder when migrations were skipped.
//
// This is the seam that owns the I/O half: which logger the lines go to, and
// which of the four optional sections have anything to say this run.
func (a *App) report(order []*container.Instance, migrate bool) {
	l := a.log()
	l.Info(renderInstanceTable(order))
	if disabled := a.container.Disabled(); len(disabled) > 0 {
		l.Info(renderDisabled(disabled))
	}
	if misses := a.container.SoftMisses(); len(misses) > 0 {
		l.Info(renderSoftMisses(misses))
	}
	if note := renderMigrationNotice(order, migrate); note != "" {
		l.Info(note)
	}
}

// capabilityTokens lists an instance's detected optional capabilities in
// lifecycle order -- the order the framework will actually call them in --
// so a reader can check the sequence against what they expected to happen,
// not just the set.
//
// Only core capabilities appear here. Protocol-specific ones (routes,
// middleware) belong to the module that defines them: the core cannot name
// them without knowing about Gin, and the whole point of the split is that
// it does not.
func capabilityTokens(inst *container.Instance) []string {
	p := inst.Plugin()
	var tokens []string
	if _, ok := p.(plugin.Configurable); ok {
		tokens = append(tokens, "config")
	}
	if _, ok := p.(plugin.Initializer); ok {
		tokens = append(tokens, "init")
	}
	if _, ok := p.(plugin.Migrator); ok {
		tokens = append(tokens, "migrate")
	}
	if _, ok := p.(plugin.Runner); ok {
		tokens = append(tokens, "runner")
	}
	if _, ok := p.(plugin.TrafficOpener); ok {
		tokens = append(tokens, "traffic")
	}
	if _, ok := p.(plugin.Closer); ok {
		tokens = append(tokens, "stop")
	}
	return tokens
}

// depsAndProvidesSuffix renders an instance's hard type dependencies and its
// declared products for the table's trailing column.
func depsAndProvidesSuffix(inst *container.Instance) string {
	var parts []string
	if deps := inst.Deps(); len(deps.Types) > 0 {
		ts := make([]string, 0, len(deps.Types))
		for _, d := range deps.Types {
			ts = append(ts, d.String())
		}
		parts = append(parts, "requires "+strings.Join(ts, " "))
	}
	if provides := inst.Provides(); len(provides) > 0 {
		ts := make([]string, 0, len(provides))
		for _, d := range provides {
			ts = append(ts, d.String())
		}
		parts = append(parts, "provides "+strings.Join(ts, " "))
	}
	return strings.Join(parts, "  ")
}

// renderInstanceTable renders one row per instance, with columns sized to
// this run's actual content -- a hardcoded width misaligns the moment a
// instance label or capability list is longer than whatever the author happened
// to test with.
func renderInstanceTable(order []*container.Instance) string {
	type row struct{ label, caps, suffix string }

	rows := make([]row, len(order))
	labelWidth, capsWidth := 0, 0
	for i, inst := range order {
		r := row{
			label:  inst.Label(),
			caps:   strings.Join(capabilityTokens(inst), " "),
			suffix: depsAndProvidesSuffix(inst),
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
		b.WriteString(strings.TrimRight(
			fmt.Sprintf("  %-*s  %-*s  %s", labelWidth, r.label, capsWidth, r.caps, r.suffix), " "))
	}
	return b.String()
}

// renderDisabled lists declared-but-not-enabled plugins. Being disabled is
// normal and not a warning, but "I blank-imported it and nothing happened"
// is otherwise indistinguishable from "I blank-imported it and it silently
// failed", and this line is what separates the two.
func renderDisabled(disabled []string) string {
	return fmt.Sprintf("xbc: 未启用的插件（%d）：%s\n  → 未启用不是错误；如果其中有你期望启用的，检查对应的 plugins.* 配置节",
		len(disabled), strings.Join(disabled, ", "))
}

// renderSoftMisses reports every After/Before preference whose referenced
// Definition key does not exist. Harmless to startup -- a preference pointing
// at an absent key is stale, not a broken dependency -- but a typo'd key in
// After/Before is invisible without this line, and its effect
// (the ordering you asked for silently not happening) shows up much later
// and much further away.
func renderSoftMisses(misses []topology.Miss) string {
	var b strings.Builder
	b.WriteString("xbc: 软约束未命中（不影响启动）")
	for _, m := range misses {
		dir := "After"
		if m.Dir == topology.Before {
			dir = "Before"
		}
		fmt.Fprintf(&b, "\n  %s.%s = %q —— 无此插件，忽略\n    → 拼写错误？还是忘了启用 plugins.%s？",
			m.Node, dir, m.Ref, m.Ref)
	}
	return b.String()
}

// renderMigrationNotice reports how many instances declared Migrate but had
// it skipped this run.
//
// The count is of Migrator-implementing instances, not of whatever those
// plugins would have migrated. A plugin knows how many models or tables it
// owns; the core does not know what a "model" is, and reporting a number it
// cannot compute would mean inventing one.
func renderMigrationNotice(order []*container.Instance, migrate bool) string {
	if migrate {
		return ""
	}
	n := 0
	for _, inst := range order {
		if _, ok := inst.Plugin().(plugin.Migrator); ok {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("xbc: 迁移未执行（%d 个插件声明了 Migrate，待检查）\n  → 需要迁移请使用 ./myapp migrate 或 --migrate", n)
}
