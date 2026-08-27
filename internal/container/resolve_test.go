package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/container/inject"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/topology"
)

// This file ports every still-relevant case from the old root package's
// resolve tests. The 5-pass algorithm itself (resolve.go) is a
// byte-for-byte port with only type/import substitutions, so the fixtures
// and assertions below are ported the same way -- resolveRef's Ref-matching
// goes through Ref.Type()'s real reflect.Type and a direct == comparison
// (see resolve.go's doc comments); TestResolve_RefMissing and
// TestResolve_RefInstanceNarrowedMissing exercise exactly that path and
// still pass unchanged, confirming the observable error copy did not move.
// resolve_samename_test.go carries the regression coverage for the gap this
// closed -- two distinct types that render identically via String() but
// differ via Ref.Type()'s real reflect.Type.

// ---- fake product types (marker structs, never provided by any real middleware) ----

type svcA struct{}
type svcB struct{}
type svcC struct{}

// ---- fake plugin family: each carries only the minimal fields its test needs ----

// fakeProducerA provides *svcA via tag only.
type fakeProducerA struct {
	Out *svcA `xbc:"provide"`
}

// fakeMiddle injects *svcA via tag and provides *svcB via tag -- used to chain into a linear pipeline.
type fakeMiddle struct {
	In  *svcA `xbc:"inject"`
	Out *svcB `xbc:"provide"`
}

// fakeConsumer injects *svcB via tag only -- the end of the chain.
type fakeConsumer struct {
	In *svcB `xbc:"inject"`
}

// fakeJWT stands in for a "jwt plugin" and exists purely as a target
// for Deps.Plugins' Ref matching. Its identity comes from Instance.key; it
// produces no type and satisfies a hard dependency purely by being present.
type fakeJWT struct{}

// fakeAudit uses both a tag (inject *svcA) and Dependencies()
// (plugin.RefTo("jwt")) -- verifying that resolve merges both styles into the
// same graph, so they can be freely mixed.
type fakeAudit struct {
	DB *svcA `xbc:"inject"`
}

func (p *fakeAudit) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("jwt")}}
}

// fakeOptionalConsumer's field carries optional -- missing it should neither error nor add a graph edge.
type fakeOptionalConsumer struct {
	Cache *svcC `xbc:"inject,optional"`
}

// fakeBadTag's tag action is misspelled, so inject.Scan is bound to error --
// this verifies that resolve's pass 0 propagates the scan error all the way
// out, instead of swallowing it and carrying on with empty fields.
type fakeBadTag struct {
	In *svcA `xbc:"injct"`
}

// newInst hand-assembles an *Instance, bypassing expand() and feeding it
// straight to resolve(). fields is scanned with the real inject.Scan; most
// tests below rely on resolve's own pass 0 to (re-)scan it, so pre-filling
// it here is only there to mirror the old fixture shape, not to bypass
// anything resolve itself is responsible for.
func newInst(t *testing.T, name, instanceName string, p plugin.Plugin) *Instance {
	t.Helper()
	fields, err := inject.Scan(p)
	require.NoError(t, err, "插件 %s 的 tag 扫描不应失败", name)
	return &Instance{plugin: p, key: plugin.Key(name), instance: instanceName, multiple: instanceName != defaultInstance, fields: fields}
}

// TestResolveScansTagsItself calls resolve with a bare *Instance that has no
// fields, pinning down that "scanning tags is resolve's own job" -- every
// other test goes through newInst, whose fixture has already filled in
// fields, so those tests would still pass even if resolve's pass 0 were
// deleted.
func TestResolveScansTagsItself(t *testing.T) {
	c := &Container{}
	// A bare *Instance: fields is not pre-filled, forcing resolve to scan it
	// itself. producer produces *svcA, middle consumes *svcA -- this edge exists
	// only in the tags; Dependencies() says nothing about it at all.
	producer := &Instance{plugin: &fakeProducerA{}, key: "producer-a", instance: defaultInstance}
	middle := &Instance{plugin: &fakeMiddle{}, key: "middle", instance: defaultInstance}

	order, _, err := c.resolve([]*Instance{middle, producer})
	require.NoError(t, err, "resolve 必须自己扫出 inject tag，不能依赖调用方预先填好 fields")
	require.NotEmpty(t, middle.fields, "resolve 返回后 fields 应当已被填上")
	require.Len(t, order, 2)
	assert.Equal(t, []string{"producer-a", "middle"}, idsOf(order),
		"扫出来的 inject 边要真的进图：产出方排在消费方之前")
}

func TestResolveRejectsMalformedTag(t *testing.T) {
	c := &Container{}
	bad := &Instance{plugin: &fakeBadTag{}, key: "bad", instance: defaultInstance}

	_, _, err := c.resolve([]*Instance{bad})
	require.Error(t, err, "tag 写错必须在解析阶段就报出来")
	assert.Contains(t, err.Error(), "xbc: 插件 bad 的 xbc tag 有误",
		"错误文案要点名是哪个实例的 tag 有问题")
}

func TestResolve_LinearOrder(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{})
	middle := newInst(t, "middle", defaultInstance, &fakeMiddle{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})

	// Input order is shuffled: the sorted result must not depend on the caller's order, only on the dependency graph itself.
	order, misses, err := c.resolve([]*Instance{consumer, middle, producer})
	require.NoError(t, err, "线性依赖链不应报错")
	assert.Empty(t, misses, "没有声明任何软约束，misses 必须是空的")
	require.Len(t, order, 3)
	assert.Equal(t, []string{"producer-a", "middle", "consumer"}, idsOf(order),
		"producer 产出 svcA，middle 消费 svcA 产出 svcB，consumer 消费 svcB —— 拓扑序必须是这个顺序")
}

func TestResolve_MergeTagAndDependencies(t *testing.T) {
	c := &Container{}
	jwt := newInst(t, "jwt", defaultInstance, &fakeJWT{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})
	audit := newInst(t, "audit", defaultInstance, &fakeAudit{})

	order, misses, err := c.resolve([]*Instance{audit, jwt, producer})
	require.NoError(t, err, "tag 的 svcA 依赖与 Dependencies() 的 jwt 依赖应当都被满足")
	assert.Empty(t, misses)
	// audit must sort after both jwt and producer-a -- both edges must take effect, and this assertion fails if either one is missing.
	assert.Equal(t, "audit", order[len(order)-1].ID())
}

func TestResolve_OptionalMissingLeavesZero(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "opt-consumer", defaultInstance, &fakeOptionalConsumer{})

	order, misses, err := c.resolve([]*Instance{consumer})
	require.NoError(t, err, "optional 依赖缺失不应报错")
	assert.Empty(t, misses, "optional 硬依赖缺失不算软约束未命中，不进 misses")
	require.Len(t, order, 1)

	zero, err := inject.IsZero(consumer.plugin, consumer.fields[0])
	require.NoError(t, err)
	assert.True(t, zero, "optional 缺失时字段必须留零值，框架不能塞任何东西进去")
}

// idsOf renders a topological order into a slice of id strings, so assert.Equal can compare order directly.
func idsOf(insts []*Instance) []string {
	ids := make([]string, len(insts))
	for i, inst := range insts {
		ids[i] = inst.ID()
	}
	return ids
}

// fakeDBConsumer injects *svcC via tag (default instance); no plugin provides *svcC.
type fakeDBConsumer struct {
	DB *svcC `xbc:"inject"`
}

// fakeNamedConsumer declares a named-instance dependency via Dependencies()
// -- since NeedNamed's instance name is a literal here, it could equally be
// expressed with the tag's name= option; Dependencies() is chosen here
// simply to verify this style also merges correctly into the graph, as an
// equivalent path to the tag.
type fakeNamedConsumer struct{}

func (p *fakeNamedConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Types: []plugin.Dep{plugin.NeedNamed[*svcC]("readonly")}}
}

// fakeNamedProducer provides *svcC under whatever instance name the test gives it.
type fakeNamedProducer struct {
	Out *svcC `xbc:"provide"`
}

func TestResolve_ConcreteTypeMissing(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})

	_, _, err := c.resolve([]*Instance{consumer})
	require.Error(t, err, "无人提供 *svcC，必须启动中止")
	assert.Contains(t, err.Error(), "xbc: 依赖检查失败")
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "是否忘了 import 对应的 provider/autoload 包")
	assert.NotContains(t, err.Error(), "github.com/xbcio/xbc/plugins/")
}

func TestResolve_NamedInstanceMissing(t *testing.T) {
	c := &Container{}
	report := newInst(t, "report", defaultInstance, &fakeNamedConsumer{})
	gormDefault := newInst(t, "gorm", defaultInstance, &fakeNamedProducer{})
	gormCache := newInst(t, "gorm", "cache", &fakeNamedProducer{})

	_, _, err := c.resolve([]*Instance{report, gormDefault, gormCache})
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

func (p *fakeRefConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("jwt")}}
}

// fakeNarrowedRefConsumer narrows to *fakeJWT's "readonly" instance.
type fakeNarrowedRefConsumer struct{}

func (p *fakeNarrowedRefConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("jwt").Instance("readonly")}}
}

func TestResolve_RefMissing(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := c.resolve([]*Instance{consumer})
	require.Error(t, err, "jwt 一个实例都没启用")
	// A Ref is resolved exclusively from its Definition key. Neither the
	// fakeJWT concrete type nor this test's package path may affect the
	// diagnostic or the suggested configuration path.
	assert.Contains(t, err.Error(), "插件 audit 依赖插件 jwt")
	assert.Contains(t, err.Error(), "未启用")
	assert.Contains(t, err.Error(), "在 application.yml 中添加 plugins.jwt")
	assert.Contains(t, err.Error(), "配置节")
}

func TestResolve_RefInstanceNarrowedMissing(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "audit", defaultInstance, &fakeNarrowedRefConsumer{})
	jwtDefault := newInst(t, "jwt", defaultInstance, &fakeJWT{})

	_, _, err := c.resolve([]*Instance{consumer, jwtDefault})
	require.Error(t, err, "jwt 启用了，但只有 default 实例，没有 readonly")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "[readonly]")
	assert.Contains(t, err.Error(), "当前只有")
	assert.Contains(t, err.Error(), "[default]")
	assert.Contains(t, err.Error(), "下添加 readonly 实例")
}

func TestResolve_RefWithoutInstanceDependsOnAllEnabledInstances(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})
	jwtDefault := newInst(t, "jwt", defaultInstance, &fakeJWT{})
	jwtReadonly := newInst(t, "jwt", "readonly", &fakeJWT{})

	order, misses, err := c.resolve([]*Instance{consumer, jwtReadonly, jwtDefault})
	require.NoError(t, err)
	assert.Empty(t, misses)

	positions := make(map[string]int, len(order))
	for index, inst := range order {
		positions[inst.ID()] = index
	}
	assert.Less(t, positions["jwt"], positions["audit"], "default 实例必须成为硬依赖")
	assert.Less(t, positions["jwt[readonly]"], positions["audit"], "readonly 实例也必须成为硬依赖")
}

// resolveCounter is an example of an interface defined on the consumer side: the consumer only cares about this interface, not who implements it.
type resolveCounter interface {
	Incr() int64
	Expire() error
}

// fullCounterA / fullCounterB both fully implement resolveCounter -- used to manufacture a "multiple candidates" ambiguity.
type fullCounterA struct{}

func (*fullCounterA) Incr() int64   { return 0 }
func (*fullCounterA) Expire() error { return nil }

type fullCounterB struct{}

func (*fullCounterB) Incr() int64   { return 0 }
func (*fullCounterB) Expire() error { return nil }

// halfCounterNoArgs implements only Incr, missing Expire -- used to manufacture "zero hits, but with a closest candidate".
type halfCounterNoArgs struct{}

func (*halfCounterNoArgs) Incr() int64 { return 0 }

type fakeCounterConsumer struct {
	C resolveCounter `xbc:"inject"`
}

// fakeCounterProviderConcrete declares, via Provides(), that it produces the
// concrete type *fullCounterA. It must go through Provides() (not a tag):
// tag scanning picks up a field's statically declared type, so if the field
// were declared as the interface resolveCounter, the product index would
// store the interface itself, unable to express "which concrete
// implementation was actually produced" -- a product must be a concrete
// type, with interfaces appearing only on the dependency side.
type fakeCounterProviderConcrete struct{}

func (p *fakeCounterProviderConcrete) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*fullCounterA]()}
}

func TestResolve_InterfaceSingleMatch(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "provider", defaultInstance, &fakeCounterProviderConcrete{})

	order, misses, err := c.resolve([]*Instance{consumer, producer})
	require.NoError(t, err, "唯一命中应当直接连边成功")
	assert.Empty(t, misses)
	assert.Equal(t, "ratelimit", order[len(order)-1].ID())
}

func TestResolve_InterfaceZeroMatchWithClosest(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "half-provider", defaultInstance, &fakeHalfCounterProvider{})

	_, _, err := c.resolve([]*Instance{consumer, producer})
	require.Error(t, err, "halfCounterNoArgs 没有 Expire 方法，不满足 resolveCounter")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "最接近的是")
	assert.Contains(t, err.Error(), "halfCounterNoArgs")
	assert.Contains(t, err.Error(), "缺少方法：Expire")
}

type fakeHalfCounterProvider struct{}

func (p *fakeHalfCounterProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*halfCounterNoArgs]()}
}

func TestResolve_InterfaceAmbiguous(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	pa := newInst(t, "provider-a", defaultInstance, &fakeCounterProviderConcreteA{})
	pb := newInst(t, "provider-b", defaultInstance, &fakeCounterProviderConcreteB{})

	_, _, err := c.resolve([]*Instance{consumer, pa, pb})
	require.Error(t, err, "两个候选都满足 resolveCounter，框架不能瞎猜")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "有 2 个候选")
	assert.Contains(t, err.Error(), "fullCounterA")
	assert.Contains(t, err.Error(), "fullCounterB")
	assert.Contains(t, err.Error(), `用 xbc:"inject,name=xxx" 指定实例消歧`)
}

type fakeCounterProviderConcreteA struct{}

func (p *fakeCounterProviderConcreteA) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*fullCounterA]()}
}

type fakeCounterProviderConcreteB struct{}

func (p *fakeCounterProviderConcreteB) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*fullCounterB]()}
}

// argCounter's methods match resolveCounter's method names but not their
// signatures (Incr takes a key argument, while the interface's Incr takes
// none) -- used to prove methodSignatureMatches actually rejects a
// same-named, differently-shaped method instead of treating "method exists
// by name" as good enough.
type argCounter struct{}

func (*argCounter) Incr(key string) int64 { return 0 }
func (*argCounter) Expire() error         { return nil }

type fakeArgCounterProvider struct{}

func (p *fakeArgCounterProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*argCounter]()}
}

func TestResolve_InterfaceSignatureMismatchNotSatisfied(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "arg-provider", defaultInstance, &fakeArgCounterProvider{})

	_, _, err := c.resolve([]*Instance{consumer, producer})
	require.Error(t, err, "argCounter 的 Incr 方法签名与接口不符，即便方法名都命中也不能算满足")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "最接近的是")
	assert.Contains(t, err.Error(), "argCounter")
	assert.Contains(t, err.Error(), "缺少方法：Incr")
}

// tieIncrOnly and tieExpireOnly each implement exactly one of resolveCounter's
// two methods, so against want=resolveCounter they score a tie (1 method
// hit, 1 missing).
type tieIncrOnly struct{}

func (*tieIncrOnly) Incr() int64 { return 0 }

type tieExpireOnly struct{}

func (*tieExpireOnly) Expire() error { return nil }

type fakeTieIncrProvider struct{}

func (p *fakeTieIncrProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*tieIncrOnly]()}
}

type fakeTieExpireProvider struct{}

func (p *fakeTieExpireProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*tieExpireOnly]()}
}

func TestResolve_InterfaceZeroMatchClosestTieKeepsFirstRegistered(t *testing.T) {
	// A suite with only ever one candidate registered (as in
	// TestResolve_InterfaceZeroMatchWithClosest above) can never distinguish
	// ">" from ">=" in closestProductMatch; this pins the tie-break rule down
	// directly. Mirrors registry_test.go's
	// TestRegistryClosestMatchTiesKeepEarliestRegistered.
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	incrProvider := newInst(t, "tie-incr", defaultInstance, &fakeTieIncrProvider{})
	expireProvider := newInst(t, "tie-expire", defaultInstance, &fakeTieExpireProvider{})

	_, _, err := c.resolve([]*Instance{consumer, incrProvider, expireProvider})
	require.Error(t, err, "两个候选都只命中一个方法，谁都不满足 resolveCounter")
	assert.Contains(t, err.Error(), "最接近的是")
	assert.Contains(t, err.Error(), "tieIncrOnly",
		"同分时必须保留先注册的候选 tieIncrOnly，不能被后注册的 tieExpireOnly 顶替")
	assert.NotContains(t, err.Error(), "tieExpireOnly")
	assert.Contains(t, err.Error(), "缺少方法：Expire",
		"先注册的 tieIncrOnly 缺的是 Expire")
}

func TestResolve_InterfaceZeroProductsInWholeApp(t *testing.T) {
	// No instance at all provides any product -- allProducts is empty, so
	// closestProductMatch has nothing to score and must return a nil type,
	// which resolveTypeDep renders as the "did you forget to import" hint
	// instead of a "closest candidate" one.
	c := &Container{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})

	_, _, err := c.resolve([]*Instance{consumer})
	require.Error(t, err, "整个应用没有任何插件提供任何产物，接口依赖必然无法满足")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "是否忘了 import 提供该类型的插件包")
}

// fakeMultiDefault models the default instance of a statically multi-instance
// definition: its Label() renders "multi[default]" while its ID()
// renders "multi" with no bracket. Every other fixture in this file is
// single-instance, where the two methods happen to render identically, so
// this is the only fixture that can prove resolve builds the graph with
// ID(), not Label().
type fakeMultiDefault struct{}

func TestResolve_GraphNodeUsesIDNotLabelForMultiInstanceDefault(t *testing.T) {
	c := &Container{}
	m := newInst(t, "multi", defaultInstance, &fakeMultiDefault{})
	m.multiple = true

	order, misses, err := c.resolve([]*Instance{m})
	require.NoError(t, err, "单个多实例插件的 default 实例，不该报任何错")
	assert.Empty(t, misses)
	require.Len(t, order, 1)
	// resolve maps the sorted node ids back through byID, and a map miss
	// there yields a nil *Instance rather than an error. Assert non-nil
	// before dereferencing so that building the graph with Label() fails
	// here with a readable message, instead of panicking on a nil receiver
	// and taking the rest of the package's tests down with it.
	require.NotNil(t, order[0],
		"拓扑序里出现了 nil 实例，说明建图用的 key 和 byID 的 key 对不上——"+
			"建图必须用 ID()，不能用 Label()")
	assert.Equal(t, "multi", order[0].ID(),
		"图节点必须用 ID()（不带 [default] 后缀）建图；如果建图时误用了 Label()，"+
			"这里的查找会因为 key 不匹配而失败")
}

// fakeConflictProducer and fakeConflictProducerAlt both declare producing *svcA[default] -- a conflict.
type fakeConflictProducer struct {
	Out *svcA `xbc:"provide"`
}

type fakeConflictProducerAlt struct {
	Out *svcA `xbc:"provide"`
}

func TestResolve_ProductConflict(t *testing.T) {
	c := &Container{}
	p1 := newInst(t, "conflict-a", defaultInstance, &fakeConflictProducer{})
	p2 := newInst(t, "conflict-b", defaultInstance, &fakeConflictProducerAlt{})

	_, _, err := c.resolve([]*Instance{p1, p2})
	require.Error(t, err, "两个插件的同一实例名都产出 *svcA，必须报冲突")
	assert.Contains(t, err.Error(), "conflict-a")
	assert.Contains(t, err.Error(), "conflict-b")
	assert.Contains(t, err.Error(), "都声明产出")
}

// fakeSoftAfter has no hard dependency at all; it only declares "if tracing exists, sort me after it".
type fakeSoftAfter struct{}

func (p *fakeSoftAfter) Dependencies() plugin.Deps {
	return plugin.Deps{After: []plugin.Key{"tracing"}}
}

type fakeTracing struct{}

func TestResolve_SoftConstraintOrdering(t *testing.T) {
	c := &Container{}
	// business is deliberately passed in before tracing: if After had no effect, insertion order would keep business ahead.
	business := newInst(t, "business", defaultInstance, &fakeSoftAfter{})
	tracing := newInst(t, "tracing", defaultInstance, &fakeTracing{})

	order, misses, err := c.resolve([]*Instance{business, tracing})
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"tracing", "business"}, idsOf(order),
		"After 软约束命中，必须把 tracing 排到 business 前面")
}

// fakeSoftMiss declares After on a Definition key that does not exist at all -- this must not abort startup.
type fakeSoftMiss struct{}

func (p *fakeSoftMiss) Dependencies() plugin.Deps {
	return plugin.Deps{After: []plugin.Key{"ghost"}}
}

func TestResolve_SoftConstraintMissNotFatal(t *testing.T) {
	c := &Container{}
	z := newInst(t, "z-plugin", defaultInstance, &fakeSoftMiss{})

	order, misses, err := c.resolve([]*Instance{z})
	require.NoError(t, err, "软约束引用的 Definition key 不存在，只记 miss，不能中止启动")
	require.Len(t, order, 1)
	require.Len(t, misses, 1)
	assert.Equal(t, topology.Miss{Node: "z-plugin", Ref: "ghost", Dir: topology.After}, misses[0])
}

// fakeCycleA/B/C hold hands via Dependencies() to form a cycle: a needs b, b needs c, c needs a.
type fakeCycleA struct{}

func (p *fakeCycleA) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("payment")}}
}

type fakeCycleB struct{}

func (p *fakeCycleB) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("user")}}
}

type fakeCycleC struct{}

func (p *fakeCycleC) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("order")}}
}

func TestResolve_Cycle(t *testing.T) {
	c := &Container{}
	ca := newInst(t, "user", defaultInstance, &fakeCycleA{})
	cb := newInst(t, "order", defaultInstance, &fakeCycleB{})
	cc := newInst(t, "payment", defaultInstance, &fakeCycleC{})

	_, _, err := c.resolve([]*Instance{ca, cb, cc})
	require.Error(t, err, "user → payment → order → user 是一个环")
	assert.Contains(t, err.Error(), "xbc: 依赖成环")
	assert.Contains(t, err.Error(), "→")
}

func TestResolve_MultipleErrorsReportedTogether(t *testing.T) {
	c := &Container{}
	// Two unrelated misses: user is missing *svcC, audit is missing the jwt
	// plugin. A single resolve call must report both, not return early just
	// because it hit the first one.
	userConsumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})
	auditConsumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := c.resolve([]*Instance{userConsumer, auditConsumer})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "未启用")
}

// fakeDualProvider uses both a tag (provide *svcA) and Provides() (an extra
// offer of *svcB) -- verifying that resolve also merges both styles on the
// product side, not just on the dependency side.
type fakeDualProvider struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeDualProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*svcB]()}
}

func TestResolve_ProvideTagAndProvidesMerge(t *testing.T) {
	c := &Container{}
	dual := newInst(t, "dual", defaultInstance, &fakeDualProvider{})
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{}) // injects *svcB via tag

	order, misses, err := c.resolve([]*Instance{consumer, dual})
	require.NoError(t, err, "tag 声明的 *svcA 与 Provides() 声明的 *svcB 都要生效")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"dual", "consumer"}, idsOf(order))
}

// fakeDualSameTypeProvider declares producing the exact same concrete type,
// *svcA, twice on the exact same instance -- once via tag, once via
// Provides(). Unlike fakeDualProvider above (two *different* types), this is
// a true duplicate declaration of one type, which pass 2's "other == inst"
// branch must recognize as harmless rather than as a conflict between two
// plugins.
type fakeDualSameTypeProvider struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeDualSameTypeProvider) Provides() []plugin.Dep {
	return []plugin.Dep{plugin.Offer[*svcA]()}
}

func TestResolve_ProvideTagAndProvidesSameTypeDeduped(t *testing.T) {
	c := &Container{}
	dual := newInst(t, "dual-same", defaultInstance, &fakeDualSameTypeProvider{})

	order, misses, err := c.resolve([]*Instance{dual})
	require.NoError(t, err,
		"同一实例通过 tag 与 Provides() 各声明一次同一类型，属于无害重复，不该报冲突")
	assert.Empty(t, misses)
	require.Len(t, order, 1)
	assert.Equal(t, "dual-same", order[0].ID())
}
