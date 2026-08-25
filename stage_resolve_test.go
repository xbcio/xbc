package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/internal/inject"
)

// ---- fake product types (marker structs, never provided by any real middleware) ----

type svcA struct{}
type svcB struct{}
type svcC struct{}

// ---- fake plugin family: each carries only the minimal fields its test needs ----

// fakeProducerA provides *svcA via tag only.
type fakeProducerA struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeProducerA) Name() string { return "producer-a" }

// fakeMiddle injects *svcA via tag and provides *svcB via tag -- used to chain into a linear pipeline.
type fakeMiddle struct {
	In  *svcA `xbc:"inject"`
	Out *svcB `xbc:"provide"`
}

func (p *fakeMiddle) Name() string { return "middle" }

// fakeConsumer injects *svcB via tag only -- the end of the chain.
type fakeConsumer struct {
	In *svcB `xbc:"inject"`
}

func (p *fakeConsumer) Name() string { return "consumer" }

// fakeJWT stands in for a "jwt plugin" -- it does nothing beyond Name(), and
// exists purely as a target for Deps.Plugins' Ref matching. It produces no
// type at all; it satisfies a hard dependency purely by "being present".
type fakeJWT struct{}

func (p *fakeJWT) Name() string { return "jwt" }

// fakeAudit uses both a tag (inject *svcA) and Dependencies()
// (RefOf[*fakeJWT]) -- verifying that stage 4 merges both styles into the
// same graph, so they can be freely mixed.
type fakeAudit struct {
	DB *svcA `xbc:"inject"`
}

func (p *fakeAudit) Name() string { return "audit" }
func (p *fakeAudit) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]()}}
}

// fakeOptionalConsumer's field carries optional -- missing it should neither error nor add a graph edge.
type fakeOptionalConsumer struct {
	Cache *svcC `xbc:"inject,optional"`
}

func (p *fakeOptionalConsumer) Name() string { return "opt-consumer" }

// fakeBadTag's tag action is misspelled, so inject.Scan is bound to error --
// this verifies that resolve's pass 0 propagates the scan error all the way
// out, instead of swallowing it and carrying on with empty fields.
type fakeBadTag struct {
	In *svcA `xbc:"injct"`
}

func (p *fakeBadTag) Name() string { return "bad" }

// newInst hand-assembles an *instance, bypassing Register/expand and
// feeding it straight to resolve(). fields is scanned with the real
// inject.Scan; see this Step's explanation for why.
func newInst(t *testing.T, name, instanceName string, p Plugin) *instance {
	t.Helper()
	fields, err := inject.Scan(p)
	require.NoError(t, err, "插件 %s 的 tag 扫描不应失败", name)
	return &instance{plugin: p, name: name, instance: instanceName, fields: fields}
}

// TestResolveScansTagsItself calls resolve with a bare *instance that has no
// fields, pinning down that "scanning tags is resolve's own job" -- every
// other test goes through newInst, whose fixture has already filled in
// fields, so those tests would still pass even if resolve's pass 0 were
// deleted.
func TestResolveScansTagsItself(t *testing.T) {
	a := &App{}
	// A bare *instance: fields is not pre-filled, forcing resolve to scan it
	// itself. producer produces *svcA, middle consumes *svcA -- this edge exists
	// only in the tags; Dependencies() says nothing about it at all.
	producer := &instance{plugin: &fakeProducerA{}, name: "producer-a", instance: defaultInstance}
	middle := &instance{plugin: &fakeMiddle{}, name: "middle", instance: defaultInstance}

	order, _, err := a.resolve([]*instance{middle, producer})
	require.NoError(t, err, "resolve 必须自己扫出 inject tag，不能依赖调用方预先填好 fields")
	require.NotEmpty(t, middle.fields, "resolve 返回后 fields 应当已被填上")
	require.Len(t, order, 2)
	assert.Equal(t, []string{"producer-a", "middle"}, idsOf(order),
		"扫出来的 inject 边要真的进图：产出方排在消费方之前")
}

func TestResolveRejectsMalformedTag(t *testing.T) {
	a := &App{}
	bad := &instance{plugin: &fakeBadTag{}, name: "bad", instance: defaultInstance}

	_, _, err := a.resolve([]*instance{bad})
	require.Error(t, err, "tag 写错必须在阶段 4 就报出来")
	assert.Contains(t, err.Error(), "xbc: 插件 bad 的 xbc tag 有误",
		"错误文案要点名是哪个实例的 tag 有问题")
}

func TestResolve_LinearOrder(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{})
	middle := newInst(t, "middle", defaultInstance, &fakeMiddle{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})

	// Input order is shuffled: the sorted result must not depend on the caller's order, only on the dependency graph itself.
	order, misses, err := a.resolve([]*instance{consumer, middle, producer})
	require.NoError(t, err, "线性依赖链不应报错")
	assert.Empty(t, misses, "没有声明任何软约束，misses 必须是空的")
	require.Len(t, order, 3)
	assert.Equal(t, []string{"producer-a", "middle", "consumer"}, idsOf(order),
		"producer 产出 svcA，middle 消费 svcA 产出 svcB，consumer 消费 svcB —— 拓扑序必须是这个顺序")
}

func TestResolve_MergeTagAndDependencies(t *testing.T) {
	a := &App{}
	jwt := newInst(t, "jwt", defaultInstance, &fakeJWT{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})
	audit := newInst(t, "audit", defaultInstance, &fakeAudit{})

	order, misses, err := a.resolve([]*instance{audit, jwt, producer})
	require.NoError(t, err, "tag 的 svcA 依赖与 Dependencies() 的 jwt 依赖应当都被满足")
	assert.Empty(t, misses)
	// audit must sort after both jwt and producer-a -- both edges must take effect, and this assertion fails if either one is missing.
	assert.Equal(t, "audit", order[len(order)-1].id())
}

func TestResolve_OptionalMissingLeavesZero(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "opt-consumer", defaultInstance, &fakeOptionalConsumer{})

	order, misses, err := a.resolve([]*instance{consumer})
	require.NoError(t, err, "optional 依赖缺失不应报错")
	assert.Empty(t, misses, "optional 硬依赖缺失不算软约束未命中，不进 misses")
	require.Len(t, order, 1)

	zero, err := inject.IsZero(consumer.plugin, consumer.fields[0])
	require.NoError(t, err)
	assert.True(t, zero, "optional 缺失时字段必须留零值，框架不能塞任何东西进去")
}

// idsOf renders a topological order into a slice of id strings, so assert.Equal can compare order directly.
func idsOf(insts []*instance) []string {
	ids := make([]string, len(insts))
	for i, inst := range insts {
		ids[i] = inst.id()
	}
	return ids
}

// fakeDBConsumer injects *svcC via tag (default instance); no plugin provides *svcC.
type fakeDBConsumer struct {
	DB *svcC `xbc:"inject"`
}

func (p *fakeDBConsumer) Name() string { return "user" }

// fakeNamedConsumer declares a named-instance dependency via Dependencies()
// -- since NeedNamed's instance name is a literal here, it could equally be
// expressed with the tag's name= option; Dependencies() is chosen here
// simply to verify this style also merges correctly into the graph, as an
// equivalent path to the tag.
type fakeNamedConsumer struct{}

func (p *fakeNamedConsumer) Name() string { return "report" }
func (p *fakeNamedConsumer) Dependencies() Deps {
	return Deps{Types: []Dep{NeedNamed[*svcC]("readonly")}}
}

// fakeNamedProducer provides *svcC under whatever instance name the test gives it.
type fakeNamedProducer struct {
	Out *svcC `xbc:"provide"`
}

func (p *fakeNamedProducer) Name() string { return "gorm" }

func TestResolve_ConcreteTypeMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})

	_, _, err := a.resolve([]*instance{consumer})
	require.Error(t, err, "无人提供 *svcC，必须启动中止")
	assert.Contains(t, err.Error(), "xbc: 依赖检查失败")
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "是否忘了 import github.com/xbcio/xbc/plugins/")
}

func TestResolve_NamedInstanceMissing(t *testing.T) {
	a := &App{}
	report := newInst(t, "report", defaultInstance, &fakeNamedConsumer{})
	gormDefault := newInst(t, "gorm", defaultInstance, &fakeNamedProducer{})
	gormCache := newInst(t, "gorm", "cache", &fakeNamedProducer{})

	_, _, err := a.resolve([]*instance{report, gormDefault, gormCache})
	require.Error(t, err, "只有 default/cache 实例，没有 readonly")
	assert.Contains(t, err.Error(), "插件 report 依赖")
	assert.Contains(t, err.Error(), "[readonly]")
	assert.Contains(t, err.Error(), "当前只有")
	// The "当前只有" list must enumerate both existing instances, not just the first.
	assert.Contains(t, err.Error(), "[default]")
	assert.Contains(t, err.Error(), "[cache]")
	assert.Contains(t, err.Error(), "下添加 readonly 实例")
}

// fakeRefConsumer hard-depends on the *fakeJWT plugin being present (without narrowing to an instance).
type fakeRefConsumer struct{}

func (p *fakeRefConsumer) Name() string { return "audit" }
func (p *fakeRefConsumer) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]()}}
}

// fakeNarrowedRefConsumer narrows to *fakeJWT's "readonly" instance.
type fakeNarrowedRefConsumer struct{}

func (p *fakeNarrowedRefConsumer) Name() string { return "audit" }
func (p *fakeNarrowedRefConsumer) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]().Instance("readonly")}}
}

func TestResolve_RefMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := a.resolve([]*instance{consumer})
	require.Error(t, err, "jwt 一个实例都没启用")
	// The plugin name here goes through deriveName's package-path inference
	// (fakeJWT lives in package xbc alongside this test, so the derived name is
	// necessarily "xbc" rather than "jwt" as in a real scenario) -- the assertion
	// only pins down the fixed template fragment from the contract, and never
	// compares the interpolated package name literally; see the note at the top
	// of Step 1 for why.
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "未启用")
	assert.Contains(t, err.Error(), "在 application.yml 中添加 plugins.")
	assert.Contains(t, err.Error(), "配置节")
}

func TestResolve_RefInstanceNarrowedMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "audit", defaultInstance, &fakeNarrowedRefConsumer{})
	jwtDefault := newInst(t, "jwt", defaultInstance, &fakeJWT{})

	_, _, err := a.resolve([]*instance{consumer, jwtDefault})
	require.Error(t, err, "jwt 启用了，但只有 default 实例，没有 readonly")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "[readonly]")
	assert.Contains(t, err.Error(), "当前只有")
	assert.Contains(t, err.Error(), "[default]")
	assert.Contains(t, err.Error(), "下添加 readonly 实例")
}

// Counter is an example of an interface defined on the consumer side: the consumer only cares about this interface, not who implements it.
type resolveCounter interface {
	Incr() int64
	Expire() error
}

// fullCounterA / fullCounterB both fully implement Counter -- used to manufacture a "multiple candidates" ambiguity.
type fullCounterA struct{}

func (*fullCounterA) Incr() int64   { return 0 }
func (*fullCounterA) Expire() error { return nil }

type fullCounterB struct{}

func (*fullCounterB) Incr() int64   { return 0 }
func (*fullCounterB) Expire() error { return nil }

// halfCounter implements only Incr, missing Expire -- used to manufacture "zero hits, but with a closest candidate".
type halfCounterNoArgs struct{}

func (*halfCounterNoArgs) Incr() int64 { return 0 }

type fakeCounterConsumer struct {
	C resolveCounter `xbc:"inject"`
}

func (p *fakeCounterConsumer) Name() string { return "ratelimit" }

// fakeCounterProviderConcrete declares, via Provides(), that it produces the
// concrete type *fullCounterA. It must go through Provides() (not a tag):
// tag scanning picks up a field's statically declared type, so if the field
// were declared as the interface Counter, the product index would store the
// interface itself, unable to express "which concrete implementation was
// actually produced" -- a product must be a concrete type, with interfaces
// appearing only on the dependency side. That is the other half of §5.6's
// "interface defined on the consumer side".
type fakeCounterProviderConcrete struct{}

func (p *fakeCounterProviderConcrete) Name() string { return "provider" }
func (p *fakeCounterProviderConcrete) Provides() []Dep {
	return []Dep{Offer[*fullCounterA]()}
}

func TestResolve_InterfaceSingleMatch(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "provider", defaultInstance, &fakeCounterProviderConcrete{})

	order, misses, err := a.resolve([]*instance{consumer, producer})
	require.NoError(t, err, "唯一命中应当直接连边成功")
	assert.Empty(t, misses)
	assert.Equal(t, "ratelimit", order[len(order)-1].id())
}

func TestResolve_InterfaceZeroMatchWithClosest(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "half-provider", defaultInstance, &fakeHalfCounterProvider{})

	_, _, err := a.resolve([]*instance{consumer, producer})
	require.Error(t, err, "halfCounter 没有 Expire 方法，不满足 Counter")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "最接近的是")
	assert.Contains(t, err.Error(), "halfCounter")
	assert.Contains(t, err.Error(), "缺少方法：Expire")
}

type fakeHalfCounterProvider struct{}

func (p *fakeHalfCounterProvider) Name() string { return "half-provider" }
func (p *fakeHalfCounterProvider) Provides() []Dep {
	return []Dep{Offer[*halfCounterNoArgs]()}
}

func TestResolve_InterfaceAmbiguous(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	pa := newInst(t, "provider-a", defaultInstance, &fakeCounterProviderConcreteA{})
	pb := newInst(t, "provider-b", defaultInstance, &fakeCounterProviderConcreteB{})

	_, _, err := a.resolve([]*instance{consumer, pa, pb})
	require.Error(t, err, "两个候选都满足 Counter，框架不能瞎猜")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "有 2 个候选")
	assert.Contains(t, err.Error(), "fullCounterA")
	assert.Contains(t, err.Error(), "fullCounterB")
	assert.Contains(t, err.Error(), `用 xbc:"inject,name=xxx" 指定实例消歧`)
}

type fakeCounterProviderConcreteA struct{}

func (p *fakeCounterProviderConcreteA) Name() string    { return "provider-a" }
func (p *fakeCounterProviderConcreteA) Provides() []Dep { return []Dep{Offer[*fullCounterA]()} }

type fakeCounterProviderConcreteB struct{}

func (p *fakeCounterProviderConcreteB) Name() string    { return "provider-b" }
func (p *fakeCounterProviderConcreteB) Provides() []Dep { return []Dep{Offer[*fullCounterB]()} }

// fakeConflictProducer and fakeConflictProducerAlt both declare producing *svcA[default] -- a conflict.
type fakeConflictProducer struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeConflictProducer) Name() string { return "conflict-a" }

type fakeConflictProducerAlt struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeConflictProducerAlt) Name() string { return "conflict-b" }

func TestResolve_ProductConflict(t *testing.T) {
	a := &App{}
	p1 := newInst(t, "conflict-a", defaultInstance, &fakeConflictProducer{})
	p2 := newInst(t, "conflict-b", defaultInstance, &fakeConflictProducerAlt{})

	_, _, err := a.resolve([]*instance{p1, p2})
	require.Error(t, err, "两个插件的同一实例名都产出 *svcA，必须报冲突")
	assert.Contains(t, err.Error(), "conflict-a")
	assert.Contains(t, err.Error(), "conflict-b")
	assert.Contains(t, err.Error(), "都声明产出")
}

// fakeSoftAfter has no hard dependency at all; it only declares "if tracing exists, sort me after it".
type fakeSoftAfter struct{}

func (p *fakeSoftAfter) Name() string { return "business" }
func (p *fakeSoftAfter) Dependencies() Deps {
	return Deps{After: []string{"tracing"}}
}

type fakeTracing struct{}

func (p *fakeTracing) Name() string { return "tracing" }

func TestResolve_SoftConstraintOrdering(t *testing.T) {
	a := &App{}
	// business is deliberately passed in before tracing: if After has no effect, insertion order would keep business ahead.
	business := newInst(t, "business", defaultInstance, &fakeSoftAfter{})
	tracing := newInst(t, "tracing", defaultInstance, &fakeTracing{})

	order, misses, err := a.resolve([]*instance{business, tracing})
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"tracing", "business"}, idsOf(order),
		"After 软约束命中，必须把 tracing 排到 business 前面")
}

// fakeSoftMiss declares After on a plugin name that does not exist at all -- this must not abort startup.
type fakeSoftMiss struct{}

func (p *fakeSoftMiss) Name() string { return "z-plugin" }
func (p *fakeSoftMiss) Dependencies() Deps {
	return Deps{After: []string{"ghost"}}
}

func TestResolve_SoftConstraintMissNotFatal(t *testing.T) {
	a := &App{}
	z := newInst(t, "z-plugin", defaultInstance, &fakeSoftMiss{})

	order, misses, err := a.resolve([]*instance{z})
	require.NoError(t, err, "软约束引用的名字不存在，只记 miss，不能中止启动")
	require.Len(t, order, 1)
	require.Len(t, misses, 1)
	assert.Equal(t, graph.Miss{Node: "z-plugin", Ref: "ghost", Dir: "after"}, misses[0])
}

// fakeCycleA/B/C hold hands via Dependencies() to form a cycle: a needs b, b needs c, c needs a.
type fakeCycleA struct{}

func (p *fakeCycleA) Name() string       { return "user" }
func (p *fakeCycleA) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleC]()}} }

type fakeCycleB struct{}

func (p *fakeCycleB) Name() string       { return "order" }
func (p *fakeCycleB) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleA]()}} }

type fakeCycleC struct{}

func (p *fakeCycleC) Name() string       { return "payment" }
func (p *fakeCycleC) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleB]()}} }

func TestResolve_Cycle(t *testing.T) {
	a := &App{}
	ca := newInst(t, "user", defaultInstance, &fakeCycleA{})
	cb := newInst(t, "order", defaultInstance, &fakeCycleB{})
	cc := newInst(t, "payment", defaultInstance, &fakeCycleC{})

	_, _, err := a.resolve([]*instance{ca, cb, cc})
	require.Error(t, err, "user → payment → order → user 是一个环")
	assert.Contains(t, err.Error(), "xbc: 依赖成环")
	assert.Contains(t, err.Error(), "→")
}

func TestResolve_MultipleErrorsReportedTogether(t *testing.T) {
	a := &App{}
	// Two unrelated misses: user is missing *svcC, audit is missing the jwt
	// plugin. A single resolve call must report both, not return early just
	// because it hit the first one.
	userConsumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})
	auditConsumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := a.resolve([]*instance{userConsumer, auditConsumer})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "未启用")
}

// fakeDualProvider uses both a tag (provide *svcA) and Provides() (an extra
// offer of *svcB) -- verifying that stage 4 also merges both styles on the
// product side, not just on the dependency side.
type fakeDualProvider struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeDualProvider) Name() string    { return "dual" }
func (p *fakeDualProvider) Provides() []Dep { return []Dep{Offer[*svcB]()} }

func TestResolve_ProvideTagAndProvidesMerge(t *testing.T) {
	a := &App{}
	dual := newInst(t, "dual", defaultInstance, &fakeDualProvider{})
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{}) // injects *svcB via tag

	order, misses, err := a.resolve([]*instance{consumer, dual})
	require.NoError(t, err, "tag 声明的 *svcA 与 Provides() 声明的 *svcB 都要生效")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"dual", "consumer"}, idsOf(order))
}
