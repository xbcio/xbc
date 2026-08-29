package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/assembly"
	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// --- assembly helper -------------------------------------------------------
//
// Every renderer here takes *assembly.Instance / *assembly.Container
// values, so the tests need genuinely assembled Instances. assemble below
// builds a real *assembly.Container the same way package assembly's own
// container_harness_test.go does for its package, which is what lets these tests
// exercise the renderers without going anywhere near newApp or Execute.
//
// Note what that independence does NOT come from: these tests used to live in
// their own package (internal/startupreport), and the package doc claimed the
// boundary was what made App-free testing possible. It was not. Nothing
// obliges a test in package xbc to construct the package's main type, as this
// file demonstrates by not doing so. The boundary's only real effect was
// forcing bare and def to be copied here from harness_test.go, because test
// helpers do not cross package boundaries -- both are now shared with the
// rest of the package's tests instead of duplicated.

// fakeHost forwards plugin.RuntimeHost's four methods straight back to a
// *assembly.Container, mirroring assembly/container_harness_test.go's own
// fakeHost. It is built with a nil c and wired up right after assembly.New,
// since Options.Host must already be a RuntimeHost value before the Container it
// forwards to exists.
type fakeHost struct {
	c *assembly.Container
}

func (h *fakeHost) ProvideValue(typ reflect.Type, instance string, v any) {
	h.c.ProvideValue(typ, instance, v)
}

func (h *fakeHost) LookupValue(typ reflect.Type, instance string) (any, error) {
	return h.c.LookupValue(typ, instance)
}

func (h *fakeHost) InitializedPlugins() ([]plugin.Extension[any], error) {
	return h.c.InitializedPlugins()
}

// GoManaged runs fn synchronously; none of this file's tests exercise
// Context.Go/GoCritical's own async semantics, so a synchronous stand-in is
// enough to satisfy plugin.RuntimeHost.
func (h *fakeHost) GoManaged(id plugin.Identity, fn func(context.Context), critical bool) {
	fn(context.Background())
}

// assemble builds an *assembly.Container from defs, failing the
// test immediately on any assembly error. It returns the whole Container (not
// just Order()) because the tests below also need Disabled() and SoftMisses()
// from the same assembled run.
func assemble(t *testing.T, envData map[string]any, defs ...plugin.Definition) *assembly.Container {
	t.Helper()

	cat := catalog.New()
	for _, d := range defs {
		cat.Declare(d)
	}
	snap, err := cat.Freeze()
	require.NoError(t, err, "冻结测试 catalog 失败")

	env, err := config.NewEnvironment(envData, "XBC_REPORT_TEST_")
	require.NoError(t, err, "构造内存配置环境失败")

	h := &fakeHost{}
	c := assembly.New(assembly.Options{
		Snapshot: snap,
		Env:      env,
		Host:     h,
		Logger:   log.Nop(),
	})
	h.c = c

	require.NoError(t, c.Assemble(), "测试用的插件集合应当总是能装配成功")
	return c
}

// --- narrow capability fixtures --------------------------------------------
//
// A single fake implementing every lifecycle interface at once would be
// useless for capability-detection tests: every instance built from it would
// report every capability regardless of what capabilityTokens actually does.
// These fixtures are deliberately narrow.

// fullCaps implements every optional lifecycle capability capabilityTokens
// checks for. Its methods are declared in the REVERSE of lifecycle order
// (Stop first, ConfigPtr last) on purpose: if capabilityTokens' output order
// ever tracked source declaration order instead of its own fixed,
// hand-written sequence of type assertions, this fixture would be the one to
// expose it.
type fullCaps struct{ plugin.Base }

var (
	_ plugin.Configurable  = (*fullCaps)(nil)
	_ plugin.Initializer   = (*fullCaps)(nil)
	_ plugin.Migrator      = (*fullCaps)(nil)
	_ plugin.Runner        = (*fullCaps)(nil)
	_ plugin.TrafficOpener = (*fullCaps)(nil)
	_ plugin.Closer        = (*fullCaps)(nil)
)

func (*fullCaps) Stop(context.Context) error        { return nil }
func (*fullCaps) OpenTraffic(*plugin.Context) error { return nil }
func (*fullCaps) Start(*plugin.Context) error       { return nil }
func (*fullCaps) Migrate(*plugin.Context) error     { return nil }
func (*fullCaps) Init(*plugin.Context) error        { return nil }
func (*fullCaps) ConfigPtr() any                    { return &struct{}{} }

// initAndStopOnly implements exactly two of the six optional capabilities, to
// pin that skipped capabilities leave no gap or misordering in
// capabilityTokens' output -- only "init" and "stop" should ever appear for
// it, in that relative order.
type initAndStopOnly struct{ plugin.Base }

var (
	_ plugin.Initializer = (*initAndStopOnly)(nil)
	_ plugin.Closer      = (*initAndStopOnly)(nil)
)

func (*initAndStopOnly) Init(*plugin.Context) error { return nil }
func (*initAndStopOnly) Stop(context.Context) error { return nil }

// migratorOnly implements only Migrator, used to build a mixed set of
// instances for renderMigrationNotice's count without dragging in every other
// capability fullCaps carries.
type migratorOnly struct{ plugin.Base }

var _ plugin.Migrator = (*migratorOnly)(nil)

func (*migratorOnly) Migrate(*plugin.Context) error { return nil }

// softAfterMiss declares a soft After dependency on a Definition key that is
// never registered in any of this file's fixtures, to exercise
// renderSoftMisses' "After" rendering path.
type softAfterMiss struct{ plugin.Base }

var _ plugin.Declarer = (*softAfterMiss)(nil)

func (*softAfterMiss) Dependencies() plugin.Deps {
	return plugin.Deps{After: []plugin.Key{"ghost"}}
}

// softBeforeMiss mirrors softAfterMiss for the "Before" direction.
type softBeforeMiss struct{ plugin.Base }

var _ plugin.Declarer = (*softBeforeMiss)(nil)

func (*softBeforeMiss) Dependencies() plugin.Deps {
	return plugin.Deps{Before: []plugin.Key{"phantom"}}
}

// --- tests ------------------------------------------------------------------

// TestCapabilityTokensListsInLifecycleOrder pins capabilityTokens' central
// promise: the tokens it returns are always in lifecycle order (config, init,
// migrate, runner, traffic, stop), regardless of which subset of capabilities
// an instance actually has and regardless of the order its methods happen to
// be declared in source.
func TestCapabilityTokensListsInLifecycleOrder(t *testing.T) {
	c := assemble(t, nil,
		def("full", func() plugin.Plugin { return new(fullCaps) }),
		def("sparse", func() plugin.Plugin { return new(initAndStopOnly) }),
	)

	var full, sparse *assembly.Instance
	for _, inst := range c.Order() {
		switch inst.Label() {
		case "full":
			full = inst
		case "sparse":
			sparse = inst
		}
	}
	require.NotNil(t, full, "未找到 full 实例")
	require.NotNil(t, sparse, "未找到 sparse 实例")

	assert.Equal(t, []string{"config", "init", "migrate", "runner", "traffic", "stop"},
		capabilityTokens(full),
		"实现了全部能力接口的实例，token 顺序必须严格按生命周期顺序排列，"+
			"而不是源码里方法声明的顺序（fullCaps 的方法特意反着声明）")

	assert.Equal(t, []string{"init", "stop"}, capabilityTokens(sparse),
		"只实现部分能力接口时，缺失的能力不应该在 token 列表里留下空位或造成剩余 token 错位")
}

// TestRenderersNeverEmitRemovedDesignTokens pins the absence of every token
// that belonged to a design this framework has already moved past:
// "health"/"middleware" belong to capabilities that were split out to the web
// module, and "routes(N)"/"↑ 显式 Register" belong to the deleted
// dual-registration era. Any one of them reappearing in the report is a
// regression, not a stylistic choice, so this exercises every renderer
// together against one assembled run that also has a disabled plugin, a soft
// miss, and a skipped migration -- the three other conditions that grow extra
// report lines.
func TestRenderersNeverEmitRemovedDesignTokens(t *testing.T) {
	c := assemble(t, nil,
		def("full", func() plugin.Plugin { return new(fullCaps) }),
		def("soft_after", func() plugin.Plugin { return &softAfterMiss{} }),
		plugin.Definition{
			Key:        "shelved",
			Factory:    func() plugin.Plugin { return &bare{} },
			Activation: plugin.Configured("plugins.shelved"),
		},
	)

	combined := strings.Join([]string{
		renderInstanceTable(c.Order()),
		renderDisabled(c.Disabled()),
		renderSoftMisses(c.SoftMisses()),
		renderMigrationNotice(c.Order(), false),
	}, "\n")

	for _, removed := range []string{"health", "middleware", "routes(", "↑ 显式 Register"} {
		assert.NotContains(t, combined, removed,
			"报告输出中出现了已删除的旧设计 token %q：health/middleware 属于已拆分给 web 模块的能力，"+
				"routes(N) 与「↑ 显式 Register」属于双注册路径年代的产物，核心报告里重新出现任何一个都说明发生了回归",
			removed)
	}
}

// TestRenderInstanceTableColumnWidthAdaptsToLongestLabel pins that column
// widths are computed from this run's actual content, not from any fixed
// number. The discriminating check is on the short-named row: it must be
// padded to exactly the long name's width, computed independently in the test
// rather than copied from renderInstanceTable's own labelWidth variable, so a
// hardcoded-width regression in renderInstanceTable would make this row
// disagree with the expected string built here.
func TestRenderInstanceTableColumnWidthAdaptsToLongestLabel(t *testing.T) {
	longName := strings.Repeat("q", 61)

	c := assemble(t, nil,
		def("a", func() plugin.Plugin { return new(fullCaps) }),
		def(longName, func() plugin.Plugin { return &bare{} }),
	)
	order := c.Order()
	require.Len(t, order, 2, "预期正好两个实例")

	var short, long *assembly.Instance
	for _, inst := range order {
		switch inst.Label() {
		case "a":
			short = inst
		case longName:
			long = inst
		}
	}
	require.NotNil(t, short, "未找到短名称实例 a")
	require.NotNil(t, long, "未找到长名称实例")

	shortCaps := strings.Join(capabilityTokens(short), " ")
	require.NotEmpty(t, shortCaps, "fullCaps 应至少报告一项能力，否则这个列宽钉子测试没有区分力")

	labelWidth := len(longName)
	wantShortLine := "  a" + strings.Repeat(" ", labelWidth-len("a")) + "  " + shortCaps
	wantLongLine := "  " + longName

	table := renderInstanceTable(order)
	lines := strings.Split(table, "\n")
	require.Len(t, lines, 3, "应该是表头 + 两行实例，一共三行")

	assert.Contains(t, lines, wantShortLine,
		"短名称行必须被填充到跟长名称一样的列宽（%d），而不是某个固定宽度——"+
			"如果实现改成了硬编码宽度，这一行的填充长度就会和这里独立算出的期望值不一致", labelWidth)
	assert.Contains(t, lines, wantLongLine,
		"最长名称本身就定义了列宽，它自己这一行不应该出现任何多余的填充")
}

// TestRenderDisabledListsDeclaredButNotEnabledPlugins pins renderDisabled's
// two jobs: naming every declared-but-inactive plugin, and saying plainly
// that this is not an error.
func TestRenderDisabledListsDeclaredButNotEnabledPlugins(t *testing.T) {
	c := assemble(t, nil,
		def("always_on", func() plugin.Plugin { return &bare{} }),
		plugin.Definition{
			Key:        "shelved",
			Factory:    func() plugin.Plugin { return &bare{} },
			Activation: plugin.Configured("plugins.shelved"),
		},
	)

	out := renderDisabled(c.Disabled())
	assert.Contains(t, out, "shelved", "未启用的插件列表里应包含 shelved")
	assert.Contains(t, out, "未启用不是错误", "应该明确告诉读者未启用不是错误")
	assert.NotContains(t, out, "always_on", "已启用的插件不应该出现在未启用列表里")
}

// TestRenderSoftMissesRendersDirectionsWithHints pins renderSoftMisses' exact
// rendering of both directions: Dir is stored lowercase ("after"/"before") on
// ordering.Miss but must render capitalized, and every entry must carry the
// "typo or forgot to enable" hint naming the specific missing plugin's config
// path.
func TestRenderSoftMissesRendersDirectionsWithHints(t *testing.T) {
	c := assemble(t, nil,
		def("after_one", func() plugin.Plugin { return &softAfterMiss{} }),
		def("before_one", func() plugin.Plugin { return &softBeforeMiss{} }),
	)

	misses := c.SoftMisses()
	require.Len(t, misses, 2, "两个实例各声明了一个指向不存在插件的软约束")

	out := renderSoftMisses(misses)

	assert.Contains(t, out, `after_one.After = "ghost"`,
		"After 方向的软约束应按 Node.After = \"Ref\" 渲染，且方向词首字母大写")
	assert.Contains(t, out, "忘了启用 plugins.ghost",
		"After 未命中应给出「拼写错误？还是忘了启用」的提示，并点名具体的 plugins.* 路径")

	assert.Contains(t, out, `before_one.Before = "phantom"`,
		"Before 方向的软约束应按 Node.Before = \"Ref\" 渲染，且方向词首字母大写")
	assert.Contains(t, out, "忘了启用 plugins.phantom",
		"Before 未命中同样应给出提示，并点名具体的 plugins.* 路径")
}

// TestRenderMigrationNotice pins renderMigrationNotice's three cases:
// migrate=true always suppresses the notice regardless of how many Migrators
// exist; migrate=false reports the exact count of instances that implement
// plugin.Migrator, not the total instance count; and zero Migrators produces
// no notice even when migrate=false.
func TestRenderMigrationNotice(t *testing.T) {
	c := assemble(t, nil,
		def("has_migrate_1", func() plugin.Plugin { return new(fullCaps) }),
		def("has_migrate_2", func() plugin.Plugin { return &migratorOnly{} }),
		def("plain", func() plugin.Plugin { return &bare{} }),
	)
	order := c.Order()
	require.Len(t, order, 3, "预期三个实例：两个 Migrator，一个不是")

	assert.Equal(t, "", renderMigrationNotice(order, true),
		"migrate=true 时本次已经执行了迁移，无论有多少 Migrator 都不应该再提示")

	note := renderMigrationNotice(order, false)
	assert.Contains(t, note, "（2 个插件声明了 Migrate",
		"应该准确统计实现了 Migrator 接口的实例数（2 个），而不是全部实例数（3 个）")
	assert.Contains(t, note, "--migrate", "应该提示如何手动触发迁移")

	noMigrators := assemble(t, nil, def("plain_only", func() plugin.Plugin { return &bare{} }))
	assert.Equal(t, "", renderMigrationNotice(noMigrators.Order(), false),
		"没有任何实例实现 Migrator 时，即使 migrate=false 也不应该产生迁移提示")
}
