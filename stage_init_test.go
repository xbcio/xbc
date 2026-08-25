package xbc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/inject"
)

// fakeConn and fakeWidget stand in for what a real plugin (gorm, redis, ...)
// would provide: a pointer type whose zero value is nil, so the harvest-time
// zero check below has something meaningful to catch.
type fakeConn struct{ id string }
type fakeWidget struct{ n int }

func newInitTestApp(t *testing.T) *App {
	t.Helper()
	return &App{
		cfg:      &Config{Server: ServerConfig{ShutdownTimeout: 2 * time.Second}},
		registry: newRegistry(),
	}
}

// mustInstance builds a stage-5-ready *instance the same way the real
// pipeline does: it routes through a.newInstance (stage 2's constructor) so
// inst.ctx is the genuine, App-bound *Context wired into the plugin's
// embedded Base -- not a hand-rolled approximation that only exists in
// tests. inst.fields is filled the same way stage 4's resolve fills it: one
// inject.Scan, cached on the instance.
func mustInstance(t *testing.T, a *App, p Plugin, name, instName string) *instance {
	t.Helper()
	fs, err := inject.Scan(p)
	require.NoError(t, err, "扫描插件 %s 的 tag 失败", name)
	inst := a.newInstance(p, name, instName, sourceRegister)
	inst.fields = fs
	return inst
}

// ---- plugin fixtures ----

type providerPlugin struct {
	Base
	Conn    *fakeConn `xbc:"provide"`
	forget  bool      // Init "forgets" to set Conn -- harvest must catch this
	failErr error
}

func (p *providerPlugin) Name() string { return "gorm" }
func (p *providerPlugin) Init(ctx *Context) error {
	if p.failErr != nil {
		return p.failErr
	}
	if p.forget {
		return nil
	}
	p.Conn = &fakeConn{id: ctx.Instance()}
	return nil
}

type consumerPlugin struct {
	Base
	Conn *fakeConn `xbc:"inject"`
}

func (p *consumerPlugin) Name() string            { return "user" }
func (p *consumerPlugin) Init(ctx *Context) error { return nil }

type optionalConsumerPlugin struct {
	Base
	Conn *fakeConn `xbc:"inject,optional"`
}

func (p *optionalConsumerPlugin) Name() string { return "report" }

type namedConsumerPlugin struct {
	Base
	RO *fakeConn `xbc:"inject,name=readonly"`
}

func (p *namedConsumerPlugin) Name() string { return "audit" }

type manualProviderPlugin struct {
	Base
	registerOnInit bool
}

func (p *manualProviderPlugin) Name() string    { return "cache" }
func (p *manualProviderPlugin) Provides() []Dep { return []Dep{Offer[*fakeWidget]()} }
func (p *manualProviderPlugin) Init(ctx *Context) error {
	if p.registerOnInit {
		Provide(ctx, &fakeWidget{n: 1})
	}
	return nil
}

// provideOnlyPlugin deliberately does not implement Initializer -- its
// provide field is set by the constructor instead, the way a single-instance
// plugin registered via app.Register is allowed to under ruling R7.
type provideOnlyPlugin struct {
	Base
	Widget *fakeWidget `xbc:"provide"`
}

func (p *provideOnlyPlugin) Name() string { return "widget" }

// provideAndDeclarePlugin declares the same type through both provide
// channels at once: a "provide" tag field (harvested by harvestInstance)
// and a Provides() entry (checked by validateManualProvides). Init only
// sets the tagged field -- it never calls xbc.Provide manually -- so
// validateManualProvides's registry lookup can only succeed if
// harvestInstance has already run and put the field's value into the
// registry. This is the fixture that pins down harvest-before-validate
// ordering: swapping the two steps makes this test fail even though every
// other test in this file still passes, because none of them declares
// Provides() for a type that is *only* satisfied via a provide tag.
type provideAndDeclarePlugin struct {
	Base
	Widget *fakeWidget `xbc:"provide"`
}

func (p *provideAndDeclarePlugin) Name() string    { return "combo" }
func (p *provideAndDeclarePlugin) Provides() []Dep { return []Dep{Offer[*fakeWidget]()} }
func (p *provideAndDeclarePlugin) Init(ctx *Context) error {
	p.Widget = &fakeWidget{n: 9}
	return nil
}

type stoppablePlugin struct {
	Base
	name      string
	stopped   *[]string
	stopErr   error
	stopPanic bool
}

func (p *stoppablePlugin) Name() string        { return p.name }
func (p *stoppablePlugin) Init(*Context) error { return nil }
func (p *stoppablePlugin) Stop(context.Context) error {
	*p.stopped = append(*p.stopped, p.name)
	if p.stopPanic {
		panic("模拟 Stop panic：" + p.name)
	}
	return p.stopErr
}

// ---- tests ----

// Covers both "injecting the default instance" and "a provide tag being
// harvested with the downstream able to inject it": gorm[default] produces a
// connection, user depends on it, and both steps are verified chained
// together in one initAll call.
func TestProvideHarvestedAndInjectedToDownstreamDefaultInstance(t *testing.T) {
	a := newInitTestApp(t)
	prov := &providerPlugin{}
	provInst := mustInstance(t, a, prov, "gorm", "default")
	cons := &consumerPlugin{}
	consInst := mustInstance(t, a, cons, "user", "default")

	require.NoError(t, a.initAll([]*instance{provInst, consInst}))
	require.NotNil(t, cons.Conn, "user 应该拿到 gorm 产出的连接")
	assert.Equal(t, "default", cons.Conn.id)
}

func TestInjectNamedInstance(t *testing.T) {
	a := newInitTestApp(t)
	prov := &providerPlugin{}
	provInst := mustInstance(t, a, prov, "gorm", "readonly")
	cons := &namedConsumerPlugin{}
	consInst := mustInstance(t, a, cons, "audit", "default")

	require.NoError(t, a.initAll([]*instance{provInst, consInst}))
	require.NotNil(t, cons.RO)
	assert.Equal(t, "readonly", cons.RO.id)
}

func TestInjectOptionalMissingLeavesZeroValue(t *testing.T) {
	a := newInitTestApp(t)
	cons := &optionalConsumerPlugin{}
	consInst := mustInstance(t, a, cons, "report", "default")

	require.NoError(t, a.initAll([]*instance{consInst}))
	assert.Nil(t, cons.Conn, "可选依赖缺失时留零值，不报错")
}

// This is the fallback described in the task rationale: stage 4 should have
// already caught this, so seeing it here means the two stages disagree --
// that must fail loudly, not silently inject a nil.
func TestInjectRequiredMissingIsTreatedAsInternalError(t *testing.T) {
	a := newInitTestApp(t)
	cons := &consumerPlugin{}
	consInst := mustInstance(t, a, cons, "user", "default")

	err := a.initAll([]*instance{consInst})
	require.Error(t, err, "阶段 4 本该拦住这种缺失，阶段 5 兜底同样要报错，不能让 nil 溜过去")
	assert.Contains(t, err.Error(), "内部错误")
}

// The fake types in this test verify the error message's template and
// layout; the placeholders are filled with this test's own plugin labels
// and type names -- kernel tests are not allowed to import real gorm, so the
// literal string "*gorm.DB" can never appear.
func TestHarvestZeroValueErrorMessageVerbatim(t *testing.T) {
	a := newInitTestApp(t)
	prov := &providerPlugin{forget: true}
	provInst := mustInstance(t, a, prov, "gorm", "readonly")

	err := a.initAll([]*instance{provInst})
	require.Error(t, err)
	want := "xbc: 插件 gorm[readonly] 声明产出 *xbc.fakeConn，但 Init 后该字段仍为 nil\n" +
		"  → 检查 Init 中是否忘记给 Conn 字段赋值"
	assert.Equal(t, want, err.Error())
}

func TestProvidesDeclaredButNotManuallyRegisteredFails(t *testing.T) {
	a := newInitTestApp(t)
	p := &manualProviderPlugin{registerOnInit: false}
	inst := mustInstance(t, a, p, "cache", "default")

	err := a.initAll([]*instance{inst})
	require.Error(t, err, "Provides() 声明了产出，但 Init 里没有调用 xbc.Provide，必须报错")
	assert.Contains(t, err.Error(), "cache")
	assert.Contains(t, err.Error(), "xbc.Provide")
}

func TestProvidesWithManualProvideSucceeds(t *testing.T) {
	a := newInitTestApp(t)
	p := &manualProviderPlugin{registerOnInit: true}
	inst := mustInstance(t, a, p, "cache", "default")

	require.NoError(t, a.initAll([]*instance{inst}))
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 1}, got)
}

func TestMultiInstanceProductKeysDoNotCollide(t *testing.T) {
	a := newInitTestApp(t)
	def := &providerPlugin{}
	ro := &providerPlugin{}
	defInst := mustInstance(t, a, def, "gorm", "default")
	roInst := mustInstance(t, a, ro, "gorm", "readonly")

	require.NoError(t, a.initAll([]*instance{defInst, roInst}))

	gotDef, err := a.registry.lookup(typeOf[*fakeConn](), "default")
	require.NoError(t, err)
	assert.Equal(t, "default", gotDef.(*fakeConn).id, "default 实例的产物必须能按 default 键取到")

	gotRO, err := a.registry.lookup(typeOf[*fakeConn](), "readonly")
	require.NoError(t, err)
	assert.Equal(t, "readonly", gotRO.(*fakeConn).id, "readonly 实例的产物必须能按 readonly 键取到，不能串成 default 的")
}

// Offer[T]() always builds Dep{Instance: ""} -- Provides() has no way to
// name which instance produces what, since that is decided by which
// instance the plugin itself is. validateManualProvides must therefore
// check the registry under the *instance's own name* (inst.instance), never
// under dep.Instance: reading dep.Instance would always mean "default" and
// silently fail to find anything a non-default instance actually
// registered.
func TestValidateManualProvidesChecksOwningInstanceNotDepInstance(t *testing.T) {
	a := newInitTestApp(t)
	p := &manualProviderPlugin{registerOnInit: true}
	inst := mustInstance(t, a, p, "cache", "readonly")

	require.NoError(t, a.initAll([]*instance{inst}),
		"Provides() 声明的 Dep 的实例名恒为空串，校验必须按插件自己的实例名 readonly 去查")
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "readonly")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 1}, got)
}

func TestRollbackStopsInReverseOrderOnInitFailure(t *testing.T) {
	a := newInitTestApp(t)
	var stopped []string

	aPlugin := &stoppablePlugin{name: "a", stopped: &stopped}
	bPlugin := &stoppablePlugin{name: "b", stopped: &stopped}
	cPlugin := &providerPlugin{failErr: errors.New("模拟 c 初始化失败")}
	// d sits after the failing c and is a Closer, so it is the only fixture
	// element that can tell rollback's "skip anything not inited" guard apart
	// from no guard at all. Without it, dropping the guard changes nothing
	// observable: c is not a Closer, so stopInstanceSafely returns early for
	// it either way, and every instance ahead of c really was inited.
	dPlugin := &stoppablePlugin{name: "d", stopped: &stopped}

	aInst := mustInstance(t, a, aPlugin, "a", "default")
	bInst := mustInstance(t, a, bPlugin, "b", "default")
	cInst := mustInstance(t, a, cPlugin, "gorm", "default")
	dInst := mustInstance(t, a, dPlugin, "d", "default")

	err := a.initAll([]*instance{aInst, bInst, cInst, dInst})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟 c 初始化失败")
	assert.Equal(t, []string{"b", "a"}, stopped,
		"已成功 Init 的插件必须按拓扑序的逆序 Stop；d 排在失败的 c 之后，压根没轮到 Init，"+
			"绝不能被 Stop——对一个从未初始化的插件调 Stop 正是空指针的经典来源")
	assert.False(t, cInst.inited, "c 自己 Init 失败，不能标记为已初始化")
	assert.False(t, dInst.inited, "d 根本没轮到 Init，不能标记为已初始化")
}

func TestRollbackStopErrorAndPanicDoNotMaskOriginalError(t *testing.T) {
	a := newInitTestApp(t)
	var stopped []string

	aPlugin := &stoppablePlugin{name: "a", stopped: &stopped, stopErr: errors.New("a 的 Stop 自己也炸了")}
	bPlugin := &stoppablePlugin{name: "b", stopped: &stopped, stopPanic: true}
	cPlugin := &providerPlugin{failErr: errors.New("原始错误：模拟初始化失败")}

	aInst := mustInstance(t, a, aPlugin, "a", "default")
	bInst := mustInstance(t, a, bPlugin, "b", "default")
	cInst := mustInstance(t, a, cPlugin, "gorm", "default")

	var err error
	assert.NotPanics(t, func() {
		err = a.initAll([]*instance{aInst, bInst, cInst})
	}, "回滚中一个插件的 Stop panic 不能让整个回滚流程崩掉")
	require.Error(t, err)
	assert.Equal(t, "xbc: 插件 gorm 初始化失败: 原始错误：模拟初始化失败", err.Error(),
		"回滚阶段任何 Stop 错误或 panic 都不能覆盖最初触发回滚的错误")
	assert.Equal(t, []string{"b", "a"}, stopped, "b 的 panic 不能挡住 a 也被 Stop")
}

// This is the fixture from the task rationale that tells "harvest then
// validate" apart from "validate then harvest": the same type is declared
// via Provides() but only ever registered by harvestInstance reading a
// provide tag, never via a manual xbc.Provide call. If validation ran
// first, this would fail even though the plugin did everything right.
func TestValidateManualProvidesRunsAfterHarvestSoTaggedFieldsCount(t *testing.T) {
	a := newInitTestApp(t)
	p := &provideAndDeclarePlugin{}
	inst := mustInstance(t, a, p, "combo", "default")

	require.NoError(t, a.initAll([]*instance{inst}),
		"Provides() 声明的类型由 provide 字段收割进注册表，harvest 必须先于校验运行")
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 9}, got)
}

func TestSkipsInitWhenNotImplementedButStillHarvests(t *testing.T) {
	a := newInitTestApp(t)
	p := &provideOnlyPlugin{Widget: &fakeWidget{n: 7}}
	inst := mustInstance(t, a, p, "widget", "default")

	require.NoError(t, a.initAll([]*instance{inst}))
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 7}, got)
	assert.True(t, inst.inited, "没有 Init 方法不代表初始化失败，仍应视为已完成，允许后续被 Stop")
}
