package web

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin/ordering"
)

// newFixtureEntry builds an mwEntry the way Start would: a raw Middleware
// plus the owning Definition key that qualify needs. The Handler is a bare
// placeholder -- this file never dispatches an HTTP request, it only orders
// entries by name.
func newFixtureEntry(pluginKey, name string, phase Phase, after, before []string) mwEntry {
	return mwEntry{
		Middleware: Middleware{
			Name:    name,
			Phase:   phase,
			After:   after,
			Before:  before,
			Handler: func(*gin.Context) {},
		},
		qname: qualify(pluginKey, name),
	}
}

func qnames(entries []mwEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.qname
	}
	return out
}

func TestPhaseStringKnownAndOutOfRange(t *testing.T) {
	cases := []struct {
		phase Phase
		want  string
	}{
		{PhaseRecover, "recover"},
		{PhaseObserve, "observe"},
		{PhaseSecurity, "security"},
		{PhaseAuth, "auth"},
		{PhaseBusiness, "business"},
		{Phase(150), "phase(150)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.phase.String(), "Phase(%d) 的字符串形式", int(c.phase))
	}
}

// spec §4.4 的启动日志里，cors 插件的 cors 中间件、ratelimit 插件的 ratelimit
// 中间件都渲染成不带前缀的裸名字——这是"同名不加前缀"规则的两个真实例子。
func TestQualifySameNameAsPluginIsNotPrefixed(t *testing.T) {
	assert.Equal(t, "cors", qualify("cors", "cors"),
		"spec §4.4 例子：cors 插件的 cors 中间件不应显示成 cors.cors")
	assert.Equal(t, "ratelimit", qualify("ratelimit", "ratelimit"),
		"spec §4.4 例子：ratelimit 插件的 ratelimit 中间件同理")
}

// spec §4.4 的启动日志里，jwt 插件的 auth 中间件渲染成 jwt.auth——这是"不同名
// 加插件前缀"规则的真实例子。
func TestQualifyDifferentNameGetsPluginPrefix(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("jwt", "auth"),
		"spec §4.4 例子：jwt 插件的 auth 中间件应显示为 jwt.auth")
}

// spec 没有给"名字已经带点号"的真实例子，用一个合成用例验证：一个名字一旦已
// 经被限定过（不管是谁限定的），qualify 必须原样传回，绝不能再套一层插件 key 前缀。
func TestQualifyDottedNamePassesThroughUnchanged(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("audit", "jwt.auth"),
		"名字里已经带点号，说明调用方已经完成过一次限定，不能被再套一层插件 key 前缀")
}

func TestOrderMiddlewaresGroupsByPhaseAscendingRegardlessOfRegistrationOrder(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("business", "business", PhaseBusiness, nil, nil),
		newFixtureEntry("recover", "recover", PhaseRecover, nil, nil),
		newFixtureEntry("security", "security", PhaseSecurity, nil, nil),
		newFixtureEntry("auth", "auth", PhaseAuth, nil, nil),
		newFixtureEntry("observe", "observe", PhaseObserve, nil, nil),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"recover", "observe", "security", "auth", "business"}, qnames(ordered),
		"Phase 是硬边界，最终顺序必须按 Phase 升序排列，与注册顺序无关")
}

func TestOrderMiddlewaresWithinGroupRegistrationOrderIsStableAcrossManyRuns(t *testing.T) {
	build := func() []mwEntry {
		return []mwEntry{
			newFixtureEntry("c", "c", PhaseSecurity, nil, nil),
			newFixtureEntry("a", "a", PhaseSecurity, nil, nil),
			newFixtureEntry("b", "b", PhaseSecurity, nil, nil),
		}
	}
	want := []string{"c", "a", "b"}
	for i := 0; i < 100; i++ {
		ordered, misses, err := orderMiddlewares(build())
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, want, qnames(ordered), "第 %d 次运行：组内无约束的中间件必须按注册顺序稳定排列", i)
	}
}

func TestOrderMiddlewaresWithinGroupAfterConstraintTakesEffect(t *testing.T) {
	// 注册顺序是 ratelimit 先、cors 后；如果 After 没有作用，稳定排序会保持这个
	// 注册顺序。一旦 ratelimit 声明 After=cors，结果必须反过来。
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, []string{"cors"}, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"ratelimit 声明 After=cors，即便注册顺序是 ratelimit 在前，排序结果也必须把 cors 排到前面")
}

func TestOrderMiddlewaresWithinGroupBeforeConstraintTakesEffect(t *testing.T) {
	// 注册顺序是 ratelimit 先、cors 后；cors 声明 Before=ratelimit，结果必须把
	// cors 挪到 ratelimit 前面，与注册顺序相反。
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, []string{"ratelimit"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"cors 声明 Before=ratelimit，即便注册顺序是 ratelimit 在前，排序结果也必须把 cors 排到前面")
}

// spec §5.8 的 audit 例子：audit（PhaseBusiness）声明 After: "jwt.auth"
// （PhaseAuth）。300 < 400，jwt.auth 本来就排在 audit 前面，这条跨阶段约束与
// Phase 顺序一致，属于冗余声明，必须被静默忽略——既不报错，也不进 misses。
func TestOrderMiddlewaresCrossPhaseConsistentConstraintIsRedundantAndIgnored(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "audit(business) After jwt.auth(auth) 与 Phase 顺序一致，是冗余约束，不应中止启动")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"jwt.auth", "audit"}, qnames(ordered))
}

// 反过来的情形：一个 PhaseSecurity 中间件声明 After: "jwt.auth"（PhaseAuth）。
// security(200) 本来排在 auth(300) 前面，但 After 要求 jwt.auth 排在它前面——
// 方向与 Phase 顺序矛盾，必须中止启动，且错误要带上两个中间件名和两个 Phase 名。
func TestOrderMiddlewaresCrossPhaseReversedConstraintAbortsWithPhaseConflictError(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("early", "check", PhaseSecurity, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "early.check(security) After jwt.auth(auth) 与 Phase 顺序相反，必须中止启动")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict, "冲突必须能还原成 *PhaseConflictError，供上层区分于其他失败原因")
	assert.Equal(t, "early.check", conflict.From)
	assert.Equal(t, "jwt.auth", conflict.To)
	assert.Equal(t, PhaseSecurity, conflict.FromPhase)
	assert.Equal(t, PhaseAuth, conflict.ToPhase)
	assert.Contains(t, err.Error(), "early.check", "错误信息必须包含声明约束的中间件名")
	assert.Contains(t, err.Error(), "jwt.auth", "错误信息必须包含被引用的中间件名")
	assert.Contains(t, err.Error(), "security", "错误信息必须包含声明方的 Phase 名")
	assert.Contains(t, err.Error(), "auth", "错误信息必须包含被引用方的 Phase 名")
}

// 原始用例只覆盖了 After 分支的一致性检查。Before 分支（orderMiddlewares 第二
// 个循环）有独立的比较逻辑，需要单独的一致性夹具：一个 PhaseSecurity 中间件声
// 明 Before 一个 PhaseAuth 中间件。security(200) 本来就排在 auth(300) 前面，与
// Phase 顺序一致，必须被静默丢弃，路径与上面的 After 用例相同但走的是另一半代码。
func TestOrderMiddlewaresCrossPhaseConsistentBeforeConstraintIsRedundantAndIgnored(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("security", "check", PhaseSecurity, nil, []string{"jwt.auth"}),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "security.check(security) Before jwt.auth(auth) 与 Phase 顺序一致，是冗余约束，不应中止启动")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"security.check", "jwt.auth"}, qnames(ordered))
}

// 上面用例的反向版本：一个 PhaseAuth 中间件声明 Before 一个 PhaseSecurity 中间
// 件。auth(300) 本来就排在 security(200) 后面，要求排到它前面与 Phase 顺序矛
// 盾，必须中止启动，镜像 After 的反向用例，但走的是 Before 分支。
func TestOrderMiddlewaresCrossPhaseReversedBeforeConstraintAbortsWithPhaseConflictError(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("jwt", "audit", PhaseAuth, nil, []string{"security.gate"}),
		newFixtureEntry("security", "gate", PhaseSecurity, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "jwt.audit(auth) Before security.gate(security) 与 Phase 顺序相反，必须中止启动")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict, "冲突必须能还原成 *PhaseConflictError")
	assert.Equal(t, "jwt.audit", conflict.From)
	assert.Equal(t, "security.gate", conflict.To)
	assert.Equal(t, PhaseAuth, conflict.FromPhase)
	assert.Equal(t, PhaseSecurity, conflict.ToPhase)
	assert.Equal(t, ordering.Before, conflict.Dir)
	assert.Contains(t, err.Error(), "jwt.audit")
	assert.Contains(t, err.Error(), "security.gate")
	assert.Contains(t, err.Error(), "auth")
	assert.Contains(t, err.Error(), "security")
}

// 防止一种实现方式：把 Phase 顺序和 After/Before 合并成一个图，只在相邻 Phase
// 分组的"首/尾节点"之间检查边界。这里每个 Phase 放两个条目，反向约束指向后一
// 组的第二个（内部）节点 auth.a2，而不是组内第一个节点——只检查边界的实现可能
// 看不出任何矛盾，静默让 auth.a2 排到 sec.s2 前面而不中止。正确的按 Phase 分组
// 实现应该按 Phase 值本身检查每一条跨阶段引用，与组内位置无关，所以这里仍必须
// 中止。
func TestOrderMiddlewaresCrossPhaseReversedConstraintOnInteriorNodeAbortsEvenWithMultipleEntriesPerPhase(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("sec", "s1", PhaseSecurity, nil, nil),
		newFixtureEntry("sec", "s2", PhaseSecurity, []string{"auth.a2"}, nil),
		newFixtureEntry("auth", "a1", PhaseAuth, nil, nil),
		newFixtureEntry("auth", "a2", PhaseAuth, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "sec.s2(security) After auth.a2(auth) 试图把后一个 Phase 的中间件拽到前一个 Phase 之前，即便目标不是组内第一个节点，也必须中止启动")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "sec.s2", conflict.From)
	assert.Equal(t, "auth.a2", conflict.To)
	assert.Equal(t, PhaseSecurity, conflict.FromPhase)
	assert.Equal(t, PhaseAuth, conflict.ToPhase)
}

func TestOrderMiddlewaresMissingReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"tracing"}, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "引用不存在的名字是软约束未命中，不应中止启动")
	require.Len(t, misses, 1)
	assert.Equal(t, ordering.Miss{Node: "audit", Ref: "tracing", Dir: ordering.After}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

// 镜像上面 After 缺失引用的夹具，验证 Before 分支同样会记录成 miss——这份文件
// 里原本没有任何用例覆盖 Before 的悬空引用。
func TestOrderMiddlewaresMissingBeforeReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, nil, []string{"tracing"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "引用不存在的名字是软约束未命中，不应中止启动")
	require.Len(t, misses, 1)
	assert.Equal(t, ordering.Miss{Node: "audit", Ref: "tracing", Dir: ordering.Before}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

func TestOrderMiddlewaresDuplicateQualifiedNameFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "限定后重名，After/Before 靠名字引用会失去唯一性，必须报错")
	assert.Contains(t, err.Error(), "cors")
}

func TestOrderMiddlewaresCycleWithinGroupFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("a", "a", PhaseSecurity, []string{"b"}, nil),
		newFixtureEntry("b", "b", PhaseSecurity, []string{"a"}, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "a After b 且 b After a，组内出现环，必须报错")

	var cycleErr *ordering.CycleError
	assert.ErrorAs(t, err, &cycleErr, "组内成环应能还原成 ordering.CycleError")
}
