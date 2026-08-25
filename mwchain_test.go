package xbc

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
)

// newFixtureEntry builds an mwEntry the way the assembly stage would: a raw
// Middleware plus the plugin name that qualify needs. The Handler is a bare
// placeholder -- this task never dispatches an HTTP request, it only orders
// entries by name.
func newFixtureEntry(plugin, name string, phase Phase, after, before []string) mwEntry {
	return mwEntry{
		Middleware: Middleware{
			Name:    name,
			Phase:   phase,
			After:   after,
			Before:  before,
			Handler: func(*gin.Context) {},
		},
		qname:  qualify(plugin, name),
		plugin: plugin,
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

// In spec §4.4's startup log, the cors plugin's cors middleware and the
// ratelimit plugin's ratelimit middleware both render as a bare, unprefixed
// name -- two real examples of the "same name gets no prefix" rule.
func TestQualifySameNameAsPluginIsNotPrefixed(t *testing.T) {
	assert.Equal(t, "cors", qualify("cors", "cors"),
		"spec §4.4 例子：cors 插件的 cors 中间件不应显示成 cors.cors")
	assert.Equal(t, "ratelimit", qualify("ratelimit", "ratelimit"),
		"spec §4.4 例子：ratelimit 插件的 ratelimit 中间件同理")
}

// In spec §4.4's startup log, the jwt plugin's auth middleware renders as
// jwt.auth -- a real example of the "a different name gets the plugin
// prefix" rule.
func TestQualifyDifferentNameGetsPluginPrefix(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("jwt", "auth"),
		"spec §4.4 例子：jwt 插件的 auth 中间件应显示为 jwt.auth")
}

// spec gives no real example for a name that already contains a dot, so a
// synthetic case verifies it: once a name has already been qualified (no
// matter by whom), qualify must pass it through unchanged, never adding
// another layer of plugin-name prefix.
func TestQualifyDottedNamePassesThroughUnchanged(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("audit", "jwt.auth"),
		"名字里已经带点号，说明调用方已经完成过一次限定，不能被再套一层插件名前缀")
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
	// Registration order is ratelimit first, cors second; if After had no
	// effect, the stable sort would keep this registration order. Once
	// ratelimit declares After=cors, the result must be reversed.
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
	// Registration order is ratelimit first, cors second; cors declares
	// Before=ratelimit, so the result must move cors ahead of ratelimit,
	// opposite to registration order.
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

// spec §5.8's audit example: audit (PhaseBusiness) declares After:
// "jwt.auth" (PhaseAuth). 300 < 400, so jwt.auth already sorts before audit;
// this cross-phase constraint agrees with Phase order and is a redundant
// declaration, which must be silently ignored -- no error, and it must not
// land in misses either.
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

// The reverse case: a PhaseSecurity middleware declares After: "jwt.auth"
// (PhaseAuth). security(200) sorts before auth(300), but After demands
// jwt.auth sort before it -- the direction contradicts Phase order, so
// startup must abort and print both middleware names and both Phase names.
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

// The brief's own cross-phase fixtures only exercise the After branch of the
// direction check. The Before branch (orderMiddlewares' second loop) has an
// independent switch with its own comparison operators, so it needs its own
// consistent-case fixture: a PhaseSecurity middleware declares Before a
// PhaseAuth middleware. security(200) already sorts before auth(300), so
// this agrees with Phase order and must be dropped silently, exactly like
// the After case above but exercising the other code path.
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

// The reversed counterpart of the fixture above: a PhaseAuth middleware
// declares Before a PhaseSecurity middleware. auth(300) already sorts after
// security(200), so demanding to come before it contradicts Phase order and
// must abort with *PhaseConflictError, mirroring the reversed-After test but
// for the Before branch, which no other fixture in this file exercises.
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
	assert.Equal(t, "before", conflict.Dir)
	assert.Contains(t, err.Error(), "jwt.audit")
	assert.Contains(t, err.Error(), "security.gate")
	assert.Contains(t, err.Error(), "auth")
	assert.Contains(t, err.Error(), "security")
}

// Guards against an implementation that merges Phase order and After/Before
// into a single graph and only enforces the boundary between the *first* or
// *last* node of adjacent Phase groups (e.g. a chain edge between group
// endpoints). With two entries per Phase, the reversed constraint here
// targets the second (interior) node of the later group, auth.a2, not the
// group's first node -- a boundary-only implementation could fail to see
// any contradiction for this specific edge and silently let auth.a2 drift
// before sec.s2, instead of aborting. The correct per-Phase-group design
// checks every cross-phase reference by Phase value alone, regardless of
// position within its group, so this must still abort.
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
	assert.Equal(t, graph.Miss{Node: "audit", Ref: "tracing", Dir: "after"}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

// Mirrors the After-miss fixture above for the Before branch, which is
// otherwise never exercised with a dangling reference in this file.
func TestOrderMiddlewaresMissingBeforeReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, nil, []string{"tracing"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "引用不存在的名字是软约束未命中，不应中止启动")
	require.Len(t, misses, 1)
	assert.Equal(t, graph.Miss{Node: "audit", Ref: "tracing", Dir: "before"}, misses[0])
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

	var cycleErr *graph.CycleError
	assert.ErrorAs(t, err, &cycleErr, "组内成环应能还原成 internal/graph 的 CycleError")
}
