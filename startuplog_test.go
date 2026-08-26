package xbc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/log"
)

// ---- capturingLogger: records Info calls verbatim for assertions, without actually writing to a file ----

type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLogger) Debug(msg string, kv ...any) { c.add(msg) }
func (c *capturingLogger) Info(msg string, kv ...any)  { c.add(msg) }
func (c *capturingLogger) Warn(msg string, kv ...any)  { c.add(msg) }
func (c *capturingLogger) Error(msg string, kv ...any) { c.add(msg) }

// Fatal is a no-op beyond recording the message: log.Logger requires it, but
// nothing in this file exercises Fatal on this fixture, and actually exiting
// the test process here would be its own bug.
func (c *capturingLogger) Fatal(msg string, kv ...any) { c.add(msg) }

func (c *capturingLogger) With(kv ...any) log.Logger { return c }
func (c *capturingLogger) Enabled(log.Level) bool    { return true }

func (c *capturingLogger) add(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, msg)
}

func (c *capturingLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// ---- plugin fixtures: each implements only the small set of interfaces its test needs ----

type tableInitPlugin struct{ Base }

func (tableInitPlugin) Name() string               { return "gorm" }
func (tableInitPlugin) Init(*Context) error        { return nil }
func (tableInitPlugin) Stop(context.Context) error { return nil }

type tableRoutePlugin struct{ Base }

func (tableRoutePlugin) Name() string           { return "user" }
func (tableRoutePlugin) Migrate(*Context) error { return nil }
func (tableRoutePlugin) RegisterRoutes(*Router) {}

type capAllPlugin struct{ Base }

func (capAllPlugin) Name() string                 { return "all" }
func (capAllPlugin) ConfigPtr() any               { return &struct{}{} }
func (capAllPlugin) Init(*Context) error          { return nil }
func (capAllPlugin) Migrate(*Context) error       { return nil }
func (capAllPlugin) Middlewares() []Middleware    { return nil }
func (capAllPlugin) RegisterRoutes(*Router)       {}
func (capAllPlugin) Start(*Context) error         { return nil }
func (capAllPlugin) Health(context.Context) error { return nil }
func (capAllPlugin) Stop(context.Context) error   { return nil }

func TestCapabilityTokensCoverAllInterfaceCombinations(t *testing.T) {
	a := &App{routeCounts: map[string]int{"all": 3}}
	inst := &instance{plugin: capAllPlugin{}, name: "all", instance: "default"}
	assert.Equal(t,
		[]string{"config", "init", "migrate", "middleware", "routes(3)", "runner", "health", "stop"},
		capabilityTokens(a, inst))
}

func TestCapabilityTokensOnlyListsImplementedInterfaces(t *testing.T) {
	inst := &instance{plugin: tableInitPlugin{}, name: "gorm", instance: "default"}
	assert.Equal(t, []string{"init", "stop"}, capabilityTokens(&App{}, inst))
}

// TestCapabilityTokensRouteCountLookupUsesInstanceIDNotName pins routeCounts'
// lookup key to inst.id(), not inst.name. Every other fixture in this file
// uses instance: "default", where inst.id() == inst.name -- the two keys
// collide numerically, so those fixtures cannot tell an id()-keyed lookup
// apart from a name-keyed one. Using a non-default instance ("readonly")
// makes inst.id() ("user[readonly]") diverge from inst.name ("user"), so a
// lookup keyed by the wrong one reads a missing map entry and silently
// renders routes(0) instead of the real count.
func TestCapabilityTokensRouteCountLookupUsesInstanceIDNotName(t *testing.T) {
	a := &App{routeCounts: map[string]int{"user[readonly]": 7}}
	inst := &instance{plugin: tableRoutePlugin{}, name: "user", instance: "readonly"}
	assert.Equal(t, []string{"migrate", "routes(7)"}, capabilityTokens(a, inst),
		"routeCounts 必须按 inst.id()（\"user[readonly]\"）查找，不是 inst.name（\"user\"）")
}

func TestBuildPluginTableAlignsColumnsByActualContentWidth(t *testing.T) {
	gormInst := &instance{
		plugin:   tableInitPlugin{},
		name:     "gorm",
		instance: "default",
		src:      sourceImport,
		provides: []Dep{Offer[*fakeConn]()},
	}
	userInst := &instance{
		plugin:   tableRoutePlugin{},
		name:     "user",
		instance: "default",
		src:      sourceRegister,
		deps:     Deps{Types: []Dep{Need[*fakeConn]()}},
	}
	a := &App{routeCounts: map[string]int{"user": 12}}

	got := buildPluginTable(a, []*instance{gormInst, userInst})
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 4, "标题行 + gorm 一行 + user 一行 + user 的『显式 Register』标记行")

	assert.Equal(t, "xbc: 装配完成，2 个插件实例", lines[0])

	// "gorm"/"user" are both 4 characters; "migrate routes(12)" is the longest
	// capability column across the two rows, at 18 characters (verified by hand:
	// "migrate" = 7 + 1 space + "routes(12)" = 10, total 18) -- these two numbers
	// are exactly what the column width should compute to, not a number this
	// test picked by hand.
	//
	// The expected rows below are independent literal strings, not built via
	// buildPluginTable's own row formatter (fmtSprintfRow): sharing a formatter
	// between production code and a test's "want" string would leave the row
	// layout itself unpinned -- reformat the separator width or flip alignment
	// in fmtSprintfRow and both sides of the assertion would drift together,
	// staying green no matter what the actual on-screen layout became.
	const labelWidth, capsWidth = 4, 18
	wantGorm := "  gorm  init stop           provides *xbc.fakeConn"
	wantUser := "  user  migrate routes(12)  requires *xbc.fakeConn"
	assert.Equal(t, wantGorm, lines[1])
	assert.Equal(t, wantUser, lines[2])

	wantMark := strings.Repeat(" ", 2+labelWidth+2+capsWidth+2) + "↑ 显式 Register"
	assert.Equal(t, wantMark, lines[3], "user 是显式 Register，必须标出来")
}

// TestBuildPluginTableAlignsColumnsByActualContentWidthSecondFixture uses a
// second, independent pair of fixtures whose computed capsWidth (59) differs
// from the 18 above -- this specifically guards against a mutation that
// hardcodes capsWidth to 18 (or any other single constant) inside
// buildPluginTable instead of actually computing it from row content; such a
// mutation would still pass the fixture above by coincidence but must fail
// here.
func TestBuildPluginTableAlignsColumnsByActualContentWidthSecondFixture(t *testing.T) {
	// gormInst carries a non-empty suffix (provides) on purpose: fmtSprintfRow
	// pads caps out to capsWidth and then TrimRight strips trailing
	// whitespace, so a row with no suffix at all can never reveal a too-narrow
	// capsWidth -- the padding it lost is exactly the whitespace TrimRight was
	// going to remove anyway. A trailing suffix is what makes the padding gap
	// land in the middle of the string, where it cannot be trimmed away.
	gormInst := &instance{
		plugin:   tableInitPlugin{},
		name:     "gorm",
		instance: "default",
		src:      sourceImport,
		provides: []Dep{Offer[*fakeConn]()},
	}
	allInst := &instance{
		plugin:   capAllPlugin{},
		name:     "all",
		instance: "default",
		src:      sourceImport,
	}
	a := &App{routeCounts: map[string]int{"all": 3}}

	got := buildPluginTable(a, []*instance{gormInst, allInst})
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 3, "标题行 + gorm 一行 + all 一行，两者都是 sourceImport，没有『显式 Register』标记行")

	assert.Equal(t, "xbc: 装配完成，2 个插件实例", lines[0])

	// "gorm" is 4 characters, "all" is 3 -- labelWidth is 4.
	// "init stop" is 9 characters; "config init migrate middleware routes(3) runner health stop"
	// is 59 characters (verified by hand) -- capsWidth is 59, not 18.
	wantGorm := "  gorm  init stop                                                    provides *xbc.fakeConn"
	wantAll := "  all   config init migrate middleware routes(3) runner health stop"
	assert.Equal(t, wantGorm, lines[1])
	assert.Equal(t, wantAll, lines[2])
}

func TestRenderMiddlewareChainFormatsPhaseAndSoftOrder(t *testing.T) {
	chain := []mwEntry{
		{Middleware: Middleware{Name: "recovery", Phase: PhaseRecover}, qname: "xbc.recovery", plugin: "xbc"},
		{Middleware: Middleware{Name: "cors", Phase: PhaseSecurity}, qname: "cors", plugin: "cors"},
		{Middleware: Middleware{Name: "ratelimit", Phase: PhaseSecurity, After: []string{"cors"}}, qname: "ratelimit", plugin: "ratelimit"},
	}
	got := renderMiddlewareChain(chain)
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, "xbc: 中间件链（3）", lines[0])
	assert.Contains(t, lines[1], "xbc.recovery")
	assert.Contains(t, lines[1], "[recover]")
	assert.Contains(t, lines[3], "ratelimit")
	assert.Contains(t, lines[3], "after=cors")
}

// TestRenderMiddlewareChainPinsNameWidthAcrossQnameLengths closes the "third
// instance" coverage gap the final review called out: every other
// renderMiddlewareChain test above only uses assert.Contains, so a mutation
// that hardcodes nameWidth to a constant (e.g. 3) still passes them all --
// nothing ever checks the padding between the qname column and the "["
// bracket. The two qnames here differ sharply in length (4 vs 19 characters)
// specifically so that any hardcoded nameWidth would misalign at least one
// of the two lines; the expected strings below are hand-computed literals
// (see the format string "  %d. %-*s  [%s]" in renderMiddlewareChain), not
// values produced by calling the function under test, so this actually pins
// the column width instead of restating it.
func TestRenderMiddlewareChainPinsNameWidthAcrossQnameLengths(t *testing.T) {
	chain := []mwEntry{
		{Middleware: Middleware{Name: "cors", Phase: PhaseRecover}, qname: "cors", plugin: "cors"},
		{
			Middleware: Middleware{Name: "request-id-injector", Phase: PhaseSecurity, After: []string{"cors"}},
			qname:      "request-id-injector",
			plugin:     "request-id-injector",
		},
	}
	got := renderMiddlewareChain(chain)
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 3)

	assert.Equal(t, "xbc: 中间件链（2）", lines[0])
	// nameWidth must be 19 (len("request-id-injector")), not 4 (len("cors"))
	// and not any other hardcoded constant: "cors" is left-padded with 15
	// trailing spaces so the "[" columns of both lines line up.
	assert.Equal(t, "  1. cors                 [recover]", lines[1])
	assert.Equal(t, "  2. request-id-injector  [security]  after=cors", lines[2])
}

func TestRenderSoftMissesFormat(t *testing.T) {
	misses := []graph.Miss{{Node: "audit", Ref: "tracing", Dir: "after"}}
	got := renderSoftMisses(misses)
	want := "xbc: 软约束未命中（不影响启动）\n" +
		"  audit.After = \"tracing\" —— 无此插件，忽略\n" +
		"    → 拼写错误？还是忘了启用 plugins.tracing？"
	assert.Equal(t, want, got)
}

func TestRenderMigrationNoticeCountsMigratorsWhenNotMigrated(t *testing.T) {
	a := &App{migrate: false}
	insts := []*instance{
		{plugin: tableRoutePlugin{}, name: "user", instance: "default"},
		{plugin: tableInitPlugin{}, name: "gorm", instance: "default"},
	}
	got := renderMigrationNotice(a, insts)
	assert.Equal(t, "xbc: 迁移未执行（1 个插件声明了 Migrate，待检查）\n  → 需要迁移请使用 ./myapp migrate 或 --migrate", got)
}

func TestRenderMigrationNoticeEmptyWhenAlreadyMigrated(t *testing.T) {
	a := &App{migrate: true}
	insts := []*instance{{plugin: tableRoutePlugin{}, name: "user", instance: "default"}}
	assert.Empty(t, renderMigrationNotice(a, insts))
}

func TestRenderMigrationNoticeEmptyWhenNoMigrator(t *testing.T) {
	a := &App{migrate: false}
	insts := []*instance{{plugin: tableInitPlugin{}, name: "gorm", instance: "default"}}
	assert.Empty(t, renderMigrationNotice(a, insts))
}

func TestPrintStartupLogEmitsAllFourSections(t *testing.T) {
	cap := &capturingLogger{}
	log.SetLogger(cap)
	defer log.SetLogger(log.Nop())

	a := &App{
		softMisses:      []graph.Miss{{Node: "audit", Ref: "tracing", Dir: "after"}},
		middlewareChain: []mwEntry{{Middleware: Middleware{Name: "cors", Phase: PhaseSecurity}, qname: "cors"}},
	}
	insts := []*instance{{plugin: tableRoutePlugin{}, name: "user", instance: "default"}}

	a.printStartupLog(insts)

	lines := cap.snapshot()
	require.Len(t, lines, 4, "装配完成表、中间件链、软约束未命中、迁移未执行——四段缺一不可")
	assert.Contains(t, lines[0], "xbc: 装配完成")
	assert.Contains(t, lines[1], "xbc: 中间件链")
	assert.Contains(t, lines[2], "xbc: 软约束未命中")
	assert.Contains(t, lines[3], "xbc: 迁移未执行")
}
