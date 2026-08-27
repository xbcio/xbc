// app_test.go
package xbc

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// This file pins the runtime App contract independently of any one plugin's
// lifecycle: consumption of an explicitly frozen Snapshot, isolation between
// Apps in one process, startup liveness checks, and orphan configuration
// handling. Every test uses a private catalog; newApp never consults the
// process-wide default catalog.

// --- A. frozen snapshot ----------------------------------------------------

// TestAppNewUsesProvidedFrozenSnapshot pins newApp's only construction
// input: the exact private Snapshot supplied by the caller is assembled, with
// no fallback to or merge with the process-wide default catalog.
func TestAppNewUsesProvidedFrozenSnapshot(t *testing.T) {
	c := catalog.New()
	c.Declare(liveness("app-private-keepalive"))
	snapshot, err := c.Freeze()
	require.NoError(t, err)

	app := newApp(snapshot)
	app.ready = make(chan struct{})
	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)

	names := make([]string, 0, len(app.container.Order()))
	for _, inst := range app.container.Order() {
		names = append(names, inst.Label())
	}
	assert.Equal(t, []string{"app-private-keepalive"}, names,
		"newApp 必须只装配调用者提供的冻结快照")

	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)
}

// TestApp_DisabledPlugin_FactoryNeverCalled pins that the Activation gate runs
// strictly before any Factory call: a Definition whose Configured() path is
// absent from configuration must never construct a plugin value at all, not
// even to probe it and then discard the result. A wrong implementation that
// calls Factory first and only checks Activation afterwards would still
// (from the outside) produce zero instances -- the only way to tell the two
// apart is to catch the Factory call itself.
func TestApp_DisabledPlugin_FactoryNeverCalled(t *testing.T) {
	var called atomic.Bool
	app := newTestApp(t,
		plugin.Definition{
			Key:        "app-gated-plugin",
			Activation: plugin.Configured("plugins.app-gated-plugin"),
			Factory: func() plugin.Plugin {
				called.Store(true)
				return new(bare)
			},
		},
		liveness("app-gated-keepalive"),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)

	assert.False(t, called.Load(),
		"配置里没有 plugins.app-gated-plugin 小节时，禁用插件的 Factory 绝对不能被调用")
}

// --- B. App-owned runtime-state isolation --------------------------------

// appProbe is an Initializer that hands its own Init-time Context and its own
// pointer identity back to the test through onInit, and provides itself into
// the App's value registry -- exactly what a real plugin would do to make
// itself discoverable to its own dependents.
type appProbe struct {
	plugin.Base
	onInit func(ctx *plugin.Context, self *appProbe)
}

var _ plugin.Initializer = (*appProbe)(nil)

func (p *appProbe) Init(ctx *plugin.Context) error {
	if p.onInit != nil {
		p.onInit(ctx, p)
	}
	return nil
}

// TestApp_TwoApps_HaveIndependentInstancesAndValueRegistries pins the runtime
// isolation guarantee end to end: two Apps built from definitions of the
// same shape in the same process must produce distinct plugin instances,
// distinct Contexts, and distinct value registries. The registry half is the
// one that would actually catch a real regression (a package-level or
// otherwise process-shared registry, which is exactly the historical bug
// shape this design retired) -- if the two Apps' registries were not
// independent, App B's Provide call would overwrite App A's entry under the
// same (type, "default") key, and a lookup through App A's own captured
// Context would come back with App B's instance instead of its own.
func TestApp_TwoApps_HaveIndependentInstancesAndValueRegistries(t *testing.T) {
	var instA, instB *appProbe
	var ctxA, ctxB *plugin.Context

	newProbeDef := func(instOut **appProbe, ctxOut **plugin.Context) plugin.Definition {
		return def("probe", func() plugin.Plugin {
			p := &appProbe{}
			p.onInit = func(ctx *plugin.Context, self *appProbe) {
				*instOut = self
				*ctxOut = ctx
				plugin.Provide(ctx, self)
			}
			return p
		})
	}

	appA := newTestApp(t, newProbeDef(&instA, &ctxA), liveness("app-a-keepalive"))
	appB := newTestApp(t, newProbeDef(&instB, &ctxB), liveness("app-b-keepalive"))

	doneA := runAsync(appA, quietConfig(t, "")...)
	awaitReady(t, appA)
	doneB := runAsync(appB, quietConfig(t, "")...)
	awaitReady(t, appB)

	require.NotNil(t, instA)
	require.NotNil(t, instB)
	assert.NotSame(t, instA, instB,
		"两个 App 必须各自调用 Factory 产出独立的插件实例，不能共享同一个对象")

	gotA, ok := plugin.Get[*appProbe](ctxA)
	require.True(t, ok)
	assert.Same(t, instA, gotA,
		"App A 的值注册表查询必须返回 App A 自己 provide 的实例")

	gotB, ok := plugin.Get[*appProbe](ctxB)
	require.True(t, ok)
	assert.Same(t, instB, gotB,
		"App B 的值注册表查询必须返回 App B 自己 provide 的实例，不能被 App A 覆盖或读到 App A 的值")

	appA.requestStop(stopReasonSignal)
	appB.requestStop(stopReasonSignal)
	resA := awaitResult(t, doneA)
	resB := awaitResult(t, doneB)
	require.NoError(t, resA.err)
	require.NoError(t, resB.err)
}

// --- C. startup-failure judgements -----------------------------------------

// TestApp_EmptyCatalog_FailsWithBlankImportHint pins §5.2's first bullet:
// a frozen Snapshot with zero Definitions must fail startup outright, with a
// message that points at the most likely real-world cause (a missing blank
// import) rather than a generic "nothing enabled" message that would also
// describe the (very different) case covered by the next test.
func TestApp_EmptyCatalog_FailsWithBlankImportHint(t *testing.T) {
	app := newTestApp(t) // no Definitions at all

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blank import",
		"空 catalog 的错误信息必须提示大概率忘了 blank import，这是它和「声明了但没启用」之间唯一有用的区别")
}

// TestApp_NoPluginEnabled_FailsListingDisabledNames pins §5.2's second cause
// of an empty assembly: plugins were declared, but every one of them
// evaluated as disabled. The fix is different from the empty-catalog case
// (check plugins.* config, not the import list), so the error must name
// which plugins ended up disabled rather than reusing the blank-import
// message.
func TestApp_NoPluginEnabled_FailsListingDisabledNames(t *testing.T) {
	app := newTestApp(t, plugin.Definition{
		Key:        "app-never-enabled",
		Activation: plugin.Configured("plugins.app-never-enabled"),
		Factory:    func() plugin.Plugin { return new(bare) },
	})

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app-never-enabled",
		"必须点名到底是哪个插件被声明却没启用")
	assert.Contains(t, err.Error(), "没有一个被启用")
}

// --- liveness capability judgement (§5.2) ----------------------------------

// appTrafficOpenerOnly implements TrafficOpener and nothing else. Its only
// purpose is the "TrafficOpener alone is a legitimate liveness capability"
// half of assertLiveness -- the half that is easy to get backwards by
// requiring Runner specifically instead of type-asserting for either.
type appTrafficOpenerOnly struct {
	plugin.Base
	onOpen func(ctx *plugin.Context) error
}

var _ plugin.TrafficOpener = (*appTrafficOpenerOnly)(nil)

func (a *appTrafficOpenerOnly) OpenTraffic(ctx *plugin.Context) error {
	if a.onOpen != nil {
		return a.onOpen(ctx)
	}
	return nil
}

// appTaskOnlyInit implements neither Runner nor TrafficOpener, but admits one
// managed task from inside Init -- the third legitimate liveness shape
// (§5.2: "a purely background-task application can implement neither and
// just ctx.GoCritical a consumer loop from Init").
type appTaskOnlyInit struct{ plugin.Base }

var _ plugin.Initializer = (*appTaskOnlyInit)(nil)

func (a *appTaskOnlyInit) Init(ctx *plugin.Context) error {
	ctx.Go(func(context.Context) {})
	return nil
}

// TestApp_NoLivenessCapability_FailsStartup pins §5.2's core case: an
// assembled application with nothing that could ever keep it busy must fail
// fast rather than block forever on stopCh looking, to every external
// observer, like a healthy idle service.
func TestApp_NoLivenessCapability_FailsStartup(t *testing.T) {
	app := newTestApp(t, def("app-inert", func() plugin.Plugin { return new(bare) }))

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app-inert",
		"错误必须列出已装配的插件，让人一眼看出装是装上了、但没有一个能跑的")
	assert.Contains(t, err.Error(), "没有任何插件提供长期存活能力")
}

// TestApp_TrafficOpenerOnly_StartsSuccessfully is the bullet the task
// description calls out explicitly not to miss: a plugin implementing only
// TrafficOpener (no Runner) is exactly as much a live service as one
// implementing Runner, and must be accepted, not rejected for having the
// "wrong" interface shape.
func TestApp_TrafficOpenerOnly_StartsSuccessfully(t *testing.T) {
	app := newTestApp(t, def("app-edge-only", func() plugin.Plugin { return new(appTrafficOpenerOnly) }))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code,
		"只实现 TrafficOpener（不实现 Runner）的插件必须被判定为合法的存活能力，启动必须成功")
}

// TestApp_RunnerOnly_StartsSuccessfully is Runner's own half of the same
// judgement.
func TestApp_RunnerOnly_StartsSuccessfully(t *testing.T) {
	app := newTestApp(t, liveness("app-runner-only"))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)
}

// TestApp_ManagedTaskOnly_StartsSuccessfully is the third liveness shape: no
// Runner, no TrafficOpener, just a managed task admitted during Init.
func TestApp_ManagedTaskOnly_StartsSuccessfully(t *testing.T) {
	app := newTestApp(t, def("app-task-only", func() plugin.Plugin { return new(appTaskOnlyInit) }))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code,
		"Init 里用 ctx.Go 起了托管任务，即使不实现 Runner/TrafficOpener，也必须算作存活能力")
}

// --- ruling R6: orphan plugins.* sections ----------------------------------

// TestApp_OrphanConfigSection_NamesUnknownSectionAndFailsStartup pins ruling
// R6: a plugins.<key> configuration section with no matching Definition in
// the frozen Snapshot is a fatal startup error naming the orphan, not a
// silently-ignored typo.
func TestApp_OrphanConfigSection_NamesUnknownSectionAndFailsStartup(t *testing.T) {
	app := newTestApp(t, liveness("app-r6-keepalive"))

	code, err := app.Execute(context.Background(), quietConfig(t, "plugins:\n  app-typo-no-such-plugin:\n    foo: true\n"))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app-typo-no-such-plugin",
		"孤儿配置节的名字必须出现在错误信息里，否则没法定位是哪一节写错了")
	assert.Contains(t, err.Error(), "无对应插件")
}

// TestApp_DisabledPluginSection_NotTreatedAsOrphan pins the other half of
// ruling R6, the half the old implementation got wrong per this rewrite's
// own doc comment (internal/container/expand.go's checkOrphanSections):
// a plugins.<name> section for a Definition that really is declared in the
// Snapshot -- just switched off via an explicit `enabled: false` -- is not an
// orphan and must not fail startup, regardless of whether that Definition
// happened to be produced this run.
func TestApp_DisabledPluginSection_NotTreatedAsOrphan(t *testing.T) {
	app := newTestApp(t,
		def("app-known-but-disabled", func() plugin.Plugin { return new(bare) }),
		liveness("app-r6-keepalive-2"),
	)

	done := runAsync(app, quietConfig(t, "plugins:\n  app-known-but-disabled:\n    enabled: false\n")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err,
		"已链接（有 Definition）只是显式 enabled:false 的插件不是孤儿配置节，不应导致启动失败")
	assert.Equal(t, 0, res.code)
}
