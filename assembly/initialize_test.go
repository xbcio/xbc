package assembly

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/assembly/inject"
	"github.com/xbcio/xbc/plugin"
)

// This file ports the still-relevant cases from the old root package's init
// stage tests: the Inject/Init/Harvest/validateManualProvides
// lifecycle, minus rollback. Container is explicitly not responsible for
// rollback (the pinned API has no Stop-calling method at all -- that stays
// the caller's job, per the task's constraints), so none of the old
// rollback/Closer/panic-recovery tests are ported here.

// fakeConn and fakeWidget stand in for what a real plugin (gorm, redis, ...)
// would provide: a pointer type whose zero value is nil, so the
// harvest-time zero check below has something meaningful to catch.
type fakeConn struct{ id string }
type fakeWidget struct{ n int }

// mustInstance builds a resolve-ready *Instance the same way the real
// pipeline does: it routes through c.newInstance (expand's own constructor)
// so inst.ctx is the genuine, Container-bound *plugin.Context -- not a
// hand-rolled approximation that only exists in tests. inst.fields is
// filled the same way resolve fills it: one inject.Scan, cached on the
// instance.
func mustInstance(t *testing.T, c *Container, p plugin.Plugin, name, instName string) *Instance {
	t.Helper()
	fs, err := inject.Scan(p)
	require.NoError(t, err, "扫描插件 %s 的 tag 失败", name)
	inst := c.newInstance(p, plugin.Key(name), instName, instName != defaultInstance)
	inst.fields = fs
	provides, err := mergeProvides(inst)
	require.NoError(t, err, "收集插件 %s 的产出声明失败", name)
	inst.provides = provides
	return inst
}

// runLifecycle drives Inject -> Init (if implemented) -> Harvest for each
// instance in order, mirroring the shape of the old root package's initAll
// minus its rollback machinery -- rollback is deliberately out of scope for
// Container (see this file's top doc comment).
func runLifecycle(c *Container, insts []*Instance) error {
	for _, inst := range insts {
		if err := c.Inject(inst); err != nil {
			return err
		}
		if initr, ok := inst.Plugin().(plugin.Initializer); ok {
			if err := initr.Init(inst.Context()); err != nil {
				return fmt.Errorf("xbc: 插件 %s 初始化失败: %w", inst.Label(), err)
			}
		}
		if err := c.Harvest(inst); err != nil {
			return err
		}
	}
	return nil
}

// ---- plugin fixtures ----

type providerPlugin struct {
	Conn    *fakeConn `xbc:"provide"`
	forget  bool      // Init "forgets" to set Conn -- harvest must catch this
	failErr error
}

func (p *providerPlugin) Init(ctx *plugin.Context) error {
	if p.failErr != nil {
		return p.failErr
	}
	if p.forget {
		return nil
	}
	p.Conn = &fakeConn{id: ctx.Instance()}
	return nil
}

type initConsumerPlugin struct {
	Conn *fakeConn `xbc:"inject"`
}

func (p *initConsumerPlugin) Init(*plugin.Context) error { return nil }

type optionalConsumerPlugin struct {
	Conn *fakeConn `xbc:"inject,optional"`
}

type namedConsumerPlugin struct {
	RO *fakeConn `xbc:"inject,name=readonly"`
}

type manualProviderPlugin struct {
	registerOnInit bool
}

func (p *manualProviderPlugin) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*fakeWidget]()}
}
func (p *manualProviderPlugin) Init(ctx *plugin.Context) error {
	if p.registerOnInit {
		plugin.Provide(ctx, &fakeWidget{n: 1})
	}
	return nil
}

// provideOnlyPlugin deliberately does not implement plugin.Initializer --
// its provide field is set directly by the test, the way a plugin with no
// Init at all still has to be harvestable.
type provideOnlyPlugin struct {
	Widget *fakeWidget `xbc:"provide"`
}

// provideAndDeclarePlugin declares the same type through both provide
// channels at once: a "provide" tag field (harvested by Harvest's tag loop)
// and a Provides() entry (checked by validateManualProvides). Init only
// sets the tagged field -- it never calls plugin.Provide manually -- so
// validateManualProvides's registry lookup can only succeed if the tag loop
// has already run and put the field's value into the registry. This is the
// fixture that pins down harvest-then-validate ordering: swapping the two
// steps makes this test fail even though every other test in this file
// still passes, because none of them declares Provides() for a type that is
// *only* satisfied via a provide tag.
type provideAndDeclarePlugin struct {
	Widget *fakeWidget `xbc:"provide"`
}

func (p *provideAndDeclarePlugin) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*fakeWidget]()}
}
func (p *provideAndDeclarePlugin) Init(*plugin.Context) error {
	p.Widget = &fakeWidget{n: 9}
	return nil
}

// ---- tests ----

// Covers both "injecting the default instance" and "a provide tag being
// harvested with the downstream able to inject it": gorm[default] produces
// a connection, user depends on it, and both steps are verified chained
// together in one runLifecycle call.
func TestProvideHarvestedAndInjectedToDownstreamDefaultInstance(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	prov := &providerPlugin{}
	provInst := mustInstance(t, c, prov, "gorm", "default")
	cons := &initConsumerPlugin{}
	consInst := mustInstance(t, c, cons, "user", "default")

	require.NoError(t, runLifecycle(c, []*Instance{provInst, consInst}))
	require.NotNil(t, cons.Conn, "user 应该拿到 gorm 产出的连接")
	assert.Equal(t, "default", cons.Conn.id)
}

func TestInjectNamedInstance(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	prov := &providerPlugin{}
	provInst := mustInstance(t, c, prov, "gorm", "readonly")
	cons := &namedConsumerPlugin{}
	consInst := mustInstance(t, c, cons, "audit", "default")

	require.NoError(t, runLifecycle(c, []*Instance{provInst, consInst}))
	require.NotNil(t, cons.RO)
	assert.Equal(t, "readonly", cons.RO.id)
}

func TestInjectOptionalMissingLeavesZeroValue(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	cons := &optionalConsumerPlugin{}
	consInst := mustInstance(t, c, cons, "report", "default")

	require.NoError(t, runLifecycle(c, []*Instance{consInst}))
	assert.Nil(t, cons.Conn, "可选依赖缺失时留零值，不报错")
}

// This is the fallback described in initialize.go's Inject doc comment:
// resolve() should have already caught this, so seeing it here means the
// two stages disagree -- that must fail loudly, not silently inject a nil.
func TestInjectRequiredMissingIsTreatedAsInternalError(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	cons := &initConsumerPlugin{}
	consInst := mustInstance(t, c, cons, "user", "default")

	err := runLifecycle(c, []*Instance{consInst})
	require.Error(t, err, "resolve 本该拦住这种缺失，Inject 兜底同样要报错，不能让 nil 溜过去")
	assert.Contains(t, err.Error(), "内部错误")

	// The wrap must be %w, not %v: a caller catching this at a higher layer
	// should be able to errors.As into the underlying *plugin.NotFoundError
	// instead of re-parsing the message string.
	var notFound *plugin.NotFoundError
	require.ErrorAs(t, err, &notFound, "内部错误包装必须用 %w，errors.As 应该能取到底层 *plugin.NotFoundError")
}

// The fake types in this test verify the error message's template and
// layout; the placeholders are filled with this test's own plugin labels
// and type names -- kernel tests are not allowed to import real gorm, so the
// literal string "*gorm.DB" can never appear.
func TestHarvestZeroValueErrorMessageVerbatim(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	prov := &providerPlugin{forget: true}
	provInst := mustInstance(t, c, prov, "gorm", "readonly")

	err := runLifecycle(c, []*Instance{provInst})
	require.Error(t, err)
	want := "xbc: 插件 gorm[readonly] 声明产出 *assembly.fakeConn，但 Init 后该字段仍为 nil\n" +
		"  → 检查 Init 中是否忘记给 Conn 字段赋值"
	assert.Equal(t, want, err.Error())
}

func TestProvidesDeclaredButNotManuallyRegisteredFails(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	p := &manualProviderPlugin{registerOnInit: false}
	inst := mustInstance(t, c, p, "cache", "default")

	err := runLifecycle(c, []*Instance{inst})
	require.Error(t, err, "Provides() 声明了产出，但 Init 里没有调用 plugin.Provide，必须报错")
	assert.Contains(t, err.Error(), "cache")
	assert.Contains(t, err.Error(), "xbc.Provide")
}

func TestProvidesWithManualProvideSucceeds(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	p := &manualProviderPlugin{registerOnInit: true}
	inst := mustInstance(t, c, p, "cache", "default")

	require.NoError(t, runLifecycle(c, []*Instance{inst}))
	got, err := c.registry.lookup(reflect.TypeOf(&fakeWidget{}), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 1}, got)
}

func TestMultiInstanceProductKeysDoNotCollide(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	def := &providerPlugin{}
	ro := &providerPlugin{}
	defInst := mustInstance(t, c, def, "gorm", "default")
	roInst := mustInstance(t, c, ro, "gorm", "readonly")

	require.NoError(t, runLifecycle(c, []*Instance{defInst, roInst}))

	gotDef, err := c.registry.lookup(reflect.TypeOf(&fakeConn{}), "default")
	require.NoError(t, err)
	assert.Equal(t, "default", gotDef.(*fakeConn).id, "default 实例的产物必须能按 default 键取到")

	gotRO, err := c.registry.lookup(reflect.TypeOf(&fakeConn{}), "readonly")
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
	c := newTestContainer(t, freezeDefs(t), nil)
	p := &manualProviderPlugin{registerOnInit: true}
	inst := mustInstance(t, c, p, "cache", "readonly")

	require.NoError(t, runLifecycle(c, []*Instance{inst}),
		"Provides() 声明的 Dep 的实例名恒为空串，校验必须按插件自己的实例名 readonly 去查")
	got, err := c.registry.lookup(reflect.TypeOf(&fakeWidget{}), "readonly")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 1}, got)
}

// This is the fixture that tells "harvest then validate" apart from
// "validate then harvest": the same type is declared via Provides() but
// only ever registered by Harvest's tag loop reading a provide tag, never
// via a manual plugin.Provide call. If validation ran first, this would
// fail even though the plugin did everything right.
func TestValidateManualProvidesRunsAfterHarvestSoTaggedFieldsCount(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	p := &provideAndDeclarePlugin{}
	inst := mustInstance(t, c, p, "combo", "default")

	require.NoError(t, runLifecycle(c, []*Instance{inst}),
		"Provides() 声明的类型由 provide 字段收割进注册表，harvest 必须先于校验运行")
	got, err := c.registry.lookup(reflect.TypeOf(&fakeWidget{}), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 9}, got)
}

func TestSkipsInitWhenNotImplementedButStillHarvests(t *testing.T) {
	c := newTestContainer(t, freezeDefs(t), nil)
	p := &provideOnlyPlugin{Widget: &fakeWidget{n: 7}}
	inst := mustInstance(t, c, p, "widget", "default")

	require.NoError(t, runLifecycle(c, []*Instance{inst}))
	got, err := c.registry.lookup(reflect.TypeOf(&fakeWidget{}), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 7}, got)
}
