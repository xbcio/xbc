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

// forgetfulCloserPlugin's Init always succeeds -- so by the time anything
// downstream fails, it may already hold a live resource -- but it forgets
// to set its own "provide" field, so harvestInstance fails right after.
// It also implements Closer. This is the exact combination
// TestRollbackStopsInstanceWhoseInitSucceededButHarvestFailed needs: none
// of the other fixtures in this file are both "Init succeeds, a later
// step fails" and "a Closer", so nothing else can tell "inst.inited flips
// right after Init" apart from "inst.inited flips only after harvest and
// validate both succeed too".
type forgetfulCloserPlugin struct {
	Base
	Conn    *fakeConn `xbc:"provide"`
	stopped *bool
}

func (p *forgetfulCloserPlugin) Name() string        { return "leaky" }
func (p *forgetfulCloserPlugin) Init(*Context) error { return nil } // forgets to set Conn
func (p *forgetfulCloserPlugin) Stop(context.Context) error {
	*p.stopped = true
	return nil
}

// blockingStopPlugin's Stop blocks on ctx.Done() -- a deterministic signal,
// not a timed sleep -- and records that it actually woke up and returned.
// It exists to pin rollback's use of a.cfg.Server.ShutdownTimeout: without
// that deadline, ctx.Done() never fires and Stop blocks forever.
type blockingStopPlugin struct {
	Base
	name     string
	returned chan struct{}
}

func (p *blockingStopPlugin) Name() string        { return p.name }
func (p *blockingStopPlugin) Init(*Context) error { return nil }
func (p *blockingStopPlugin) Stop(ctx context.Context) error {
	<-ctx.Done()
	close(p.returned)
	return ctx.Err()
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

	// The wrap must be %w, not %v: stage_config.go/stage_resolve.go both
	// unwrap structured errors via errors.As, and this "internal error"
	// path is the same convention -- a caller catching this at a higher
	// layer should be able to errors.As into the underlying *NotFoundError
	// instead of re-parsing the message string.
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound, "内部错误包装必须用 %w，errors.As 应该能取到底层 *NotFoundError")
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
	// The wrap must be %w: errors.Is has to be able to walk through
	// initAll's "xbc: 插件 %s 初始化失败" wrapper straight back to the
	// exact error cPlugin.Init returned, the same way stage_config.go and
	// stage_resolve.go rely on errors.As/Is for their own wraps -- an
	// error message that merely *looks* the same (via %v) would satisfy
	// every string assertion here while silently breaking that chain.
	require.ErrorIs(t, err, cPlugin.failErr, "initAll 包装 Init 失败必须用 %w，errors.Is 应该能追到原始 error")
	assert.Equal(t, []string{"b", "a"}, stopped,
		"已成功 Init 的插件必须按拓扑序的逆序 Stop；d 排在失败的 c 之后，压根没轮到 Init，"+
			"绝不能被 Stop——对一个从未初始化的插件调 Stop 正是空指针的经典来源")
	assert.False(t, cInst.inited, "c 自己 Init 失败，不能标记为已初始化")
	assert.False(t, dInst.inited, "d 根本没轮到 Init，不能标记为已初始化")
}

// initAll must flip inst.inited to true right after Init succeeds, before
// harvest or validate run -- not only after every stage-5 step for that
// instance has succeeded. leaky's Init returns nil (so it may already hold
// a live resource by the time anything else fails), but its harvest step
// fails right after because it forgot to set its own provide field. If
// inited were flipped any later than "Init succeeded", rollback's "skip
// anything not inited" guard would skip Stop here too, leaking whatever
// leaky's Init had already acquired.
func TestRollbackStopsInstanceWhoseInitSucceededButHarvestFailed(t *testing.T) {
	a := newInitTestApp(t)
	var stopped bool
	leaky := &forgetfulCloserPlugin{stopped: &stopped}
	inst := mustInstance(t, a, leaky, "leaky", "default")

	err := a.initAll([]*instance{inst})
	require.Error(t, err, "忘记赋值的 provide 字段必须让 harvest 报错")
	assert.True(t, stopped,
		"Init 已经成功过，哪怕后续 harvest 才失败，回滚也必须 Stop 这个实例，否则它已经打开的资源会泄漏")
}

// rollback must bound every Stop call with a.cfg.Server.ShutdownTimeout,
// not context.Background(): blocker's Stop only returns once ctx.Done()
// fires, which is a deterministic signal tied to that deadline, not a
// timed sleep the test is gambling on. initAll itself is run on a helper
// goroutine racing against mustWaitTimeout (shared with goroutine_test.go)
// so that a regression here fails this test cleanly instead of hanging the
// whole package for its full "go test" timeout.
func TestRollbackRespectsShutdownTimeout(t *testing.T) {
	a := newInitTestApp(t)
	a.cfg.Server.ShutdownTimeout = 50 * time.Millisecond

	blocker := &blockingStopPlugin{name: "blocker", returned: make(chan struct{})}
	failing := &providerPlugin{failErr: errors.New("触发回滚：让 blocker 走到 Stop")}

	blockInst := mustInstance(t, a, blocker, "blocker", "default")
	failInst := mustInstance(t, a, failing, "gorm", "default")

	done := make(chan error, 1)
	go func() {
		done <- a.initAll([]*instance{blockInst, failInst})
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(mustWaitTimeout):
		t.Fatal("initAll 没有在超时上界内返回：rollback 大概丢了 ShutdownTimeout，" +
			"blocker 的 Stop 卡在 ctx.Done() 上永远等不到信号")
	}

	select {
	case <-blocker.returned:
	case <-time.After(mustWaitTimeout):
		t.Fatal("blocker 的 Stop 没有在超时上界内被 ctx.Done() 唤醒")
	}
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

// TestRollbackIsIdempotentAcrossRepeatedCalls pins rollback's data-level
// idempotency (ruling N4/T14-5): a second rollback call against the same
// instance must be a no-op, because the first call already cleared
// inst.inited back to false -- not because some call-site guard remembers
// "already ran". Nothing prevents shutdown/rollback from being invoked twice
// against the same instance in production (e.g. a GoCritical firing while a
// signal-triggered shutdown is already underway), and a database pool's
// Stop is not generally safe to call twice.
func TestRollbackIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	a := newInitTestApp(t)
	var stopped []string
	p := &stoppablePlugin{name: "gorm", stopped: &stopped}
	inst := mustInstance(t, a, p, "gorm", "default")
	inst.inited = true

	a.rollback([]*instance{inst})
	a.rollback([]*instance{inst})

	assert.Equal(t, []string{"gorm"}, stopped,
		"第二次 rollback 必须是 no-op：第一次已经把 inited 置回 false")
	assert.False(t, inst.inited, "rollback 成功 Stop 之后必须把 inited 清回 false")
}
