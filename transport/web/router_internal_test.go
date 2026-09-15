package web

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/extensions/authentication"
)

// nullEngine is a web.Engine that records nothing and dispatches nothing.
//
// It exists for the tests in this file and in auth_policy_test.go, which read
// the route table and the policy tiers directly and therefore have to stay in
// package web -- an internal test file cannot import transport/web/enginetest,
// because that package imports transport/web and the cycle is illegal. None of
// those tests ever serves a request: what they assert is what Handle wrote into
// the table, so an engine that accepts registrations and does nothing with them
// is the honest double. A test that needs a request actually dispatched belongs
// in package web_test, driven by enginetest.
type nullEngine struct{}

func (nullEngine) Handle(string, string, []Handler) {}
func (nullEngine) NoRoute([]Handler)                {}
func (nullEngine) NoMethod([]Handler)               {}
func (nullEngine) Serve(net.Listener) error         { return nil }
func (nullEngine) Shutdown(context.Context) error   { return nil }

var _ Engine = nullEngine{}

// newTestRouter builds a root Router over nullEngine and returns it together
// with the route table's frozen flag, which a freeze-failure test has to read.
func newTestRouter(basePath string) (*Router, *bool) {
	routes, frozen, index := newRouteTable()
	return newRouter(nullEngine{}, basePath, nil, routes, frozen, index), frozen
}

// TestGroupAuthDefaultAppliesAndIsPerRouteDeepCopy covers Router.Auth's
// group-level default and the requirement that Handle clones the policy per
// route. It inspects the mutable route table directly (this test lives in
// package web) rather than through RouteCatalog, because RouteCatalog already
// defensive-copies on every read -- that would mask a Handle that stored the
// same *AuthPolicy pointer on every route instead of cloning it.
func TestGroupAuthDefaultAppliesAndIsPerRouteDeepCopy(t *testing.T) {
	router, _ := newTestRouter("/api")

	g := router.Group("/sys_role").Auth(Accepts("jwt", "session"))
	g.GET("/list", func(context.Context, *Ctx) error { return nil })
	g.GET("/detail", func(context.Context, *Ctx) error { return nil })

	require.Len(t, *router.routes, 2)
	list := (*router.routes)[0]
	detail := (*router.routes)[1]

	require.NotNil(t, list.Auth)
	require.NotNil(t, detail.Auth)
	assert.Equal(t, []authentication.Scheme{"jwt", "session"}, list.Auth.Schemes())
	assert.Equal(t, []authentication.Scheme{"jwt", "session"}, detail.Auth.Schemes())
	assert.NotSame(t, list.Auth, detail.Auth, "组级 Auth 必须逐路由深拷贝，不能让多条路由共享同一个 *AuthPolicy")

	list.Auth.schemes[0] = "mutated"
	assert.Equal(t, authentication.Scheme("jwt"), detail.Auth.schemes[0], "修改一条路由的 Auth 方案切片不能影响另一条路由")
}

// TestGroupAuthDefaultIsTierTwoOutrankedByApplicationRule pins that a
// group-level Auth default is still only a tier-2 (route-level) declaration:
// an application-level policy rule that matches the same route must resolve
// as tier-1 and override it, exactly as it would override a per-route .Auth
// call.
func TestGroupAuthDefaultIsTierTwoOutrankedByApplicationRule(t *testing.T) {
	router, _ := newTestRouter("/api")
	g := router.Group("/sys_role").Auth(Public())
	g.GET("/list", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	route, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	require.NotNil(t, route.Auth)
	assert.True(t, route.Auth.IsPublic(), "组级 Auth 必须确实写入路由，作为 tier-2 判定的前提")

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{{
			Match:        "GET /api/sys_role/list",
			Authenticate: []authentication.Scheme{"jwt"},
		}},
	})

	got := set.resolve(route)
	assert.False(t, got.permit, "tier-1 应用规则必须能收紧组级 Auth 声明为 public 的路由")
	assert.Equal(t, tierApplicationRule, got.tier, "命中 tier-1 规则时必须报告 tierApplicationRule，而不是组级继承来的 tierRoute")
}

// TestRouteUpdatePanicsWithDiagnosticMessageForMalformedHandle guards the
// diagnostic-quality trap embedding *Router introduces: routes/frozen are now
// promoted from the embedded *Router, so a naive nil-check order in
// Route.update would dereference them through a nil *Router before ever
// reaching this guard, turning the deliberate
// "xbc: invalid route metadata handle" panic into an unhelpful bare
// nil-pointer-dereference runtime panic. A malformed handle -- one that never
// went through Handle or Match -- must still panic with the same message it
// did before the embedding.
func TestRouteUpdatePanicsWithDiagnosticMessageForMalformedHandle(t *testing.T) {
	broken := &Route{indexes: []int{0}} // *Router deliberately left nil.
	assert.PanicsWithValue(t,
		"xbc: invalid route metadata handle",
		func() { broken.Perm("whatever") },
		"嵌入 *Router 之后，格式错误的句柄仍必须给出明确诊断信息，而不是裸露的 nil 指针解引用 panic",
	)
}

// TestAppendChainNeverWritesIntoParentSpareCapacity pins appendChain's own
// contract: a derived chain must never alias its parent's spare capacity,
// because two chains derived from the same parent would then write the same
// backing array slot and silently overwrite each other's middleware -- one
// authorization boundary replaced by another's, with no compile error and no
// panic.
//
// This has to be asserted directly rather than through a route tree. Every
// parent chain on the end-to-end path has already been normalised by
// appendChain (newRouter for the root, Group for each child), so under a
// correct implementation cap always equals len there and the hazardous input
// cannot be constructed at all. Building the spare slot explicitly here keeps
// the assertion independent of how Go's allocator happens to round a slice's
// size up to a size class.
func TestAppendChainNeverWritesIntoParentSpareCapacity(t *testing.T) {
	var calls []string
	mark := func(name string) Handler {
		return func(context.Context, *Ctx) error { calls = append(calls, name); return nil }
	}

	// Spare capacity is the whole point of this input: it is the one shape
	// under which a naive append(parent, extra...) reuses parent's backing
	// array instead of allocating a fresh one.
	parent := make([]Handler, 1, 4)
	parent[0] = mark("parent")

	first := appendChain(parent, mark("first"))
	second := appendChain(parent, mark("second"))

	require.Len(t, parent, 1, "appendChain 不得改变父链的长度")
	require.Len(t, first, 2, "派生链必须是父链加上追加的处理器")
	require.Len(t, second, 2, "派生链必须是父链加上追加的处理器")

	// Invoking the handlers is what tells a copy apart from an alias: the
	// slice values themselves are functions and compare as neither equal nor
	// unequal. If appendChain wrote into parent's spare capacity, deriving
	// second overwrote the slot first still points at, so first's tail
	// reports "second" here.
	for _, handler := range []Handler{first[0], first[1], second[1]} {
		require.NoError(t, handler(context.Background(), nil))
	}
	assert.Equal(t, []string{"parent", "first", "second"}, calls,
		"派生链不得复用父链的富余容量：第一条链的尾部被第二条链覆盖了")
}
