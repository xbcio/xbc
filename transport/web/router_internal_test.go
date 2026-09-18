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
	assert.NotSame(t, list.Auth, detail.Auth, "a group-level Auth default must be deep-copied per route -- several routes must not share one *AuthPolicy")

	list.Auth.schemes[0] = "mutated"
	assert.Equal(t, authentication.Scheme("jwt"), detail.Auth.schemes[0], "Mutating one route's Auth scheme slice must not affect another route")
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
	assert.True(t, route.Auth.IsPublic(), "the group-level Auth default must really be written onto the route -- that is the premise of the tier-2 verdict below")

	set := mustPolicySet(t, SecurityConfig{
		Policies: []PolicyRule{{
			Match:        "GET /api/sys_role/list",
			Authenticate: []authentication.Scheme{"jwt"},
		}},
	})

	got := set.resolve(route)
	assert.False(t, got.permit, "a tier-1 application rule must be able to tighten a route whose group-level Auth declared it public")
	assert.Equal(t, tierApplicationRule, got.tier, "a route matched by a tier-1 rule must be reported as tierApplicationRule, not as the tierRoute it inherited from the group")
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
		"Now that *Router is embedded, a malformed handle must still produce the explicit diagnostic rather than a bare nil-pointer-dereference panic",
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

	require.Len(t, parent, 1, "appendChain must not change the parent chain's length")
	require.Len(t, first, 2, "a derived chain must be the parent chain plus the appended handler")
	require.Len(t, second, 2, "a derived chain must be the parent chain plus the appended handler")

	// Invoking the handlers is what tells a copy apart from an alias: the
	// slice values themselves are functions and compare as neither equal nor
	// unequal. If appendChain wrote into parent's spare capacity, deriving
	// second overwrote the slot first still points at, so first's tail
	// reports "second" here.
	for _, handler := range []Handler{first[0], first[1], second[1]} {
		require.NoError(t, handler(context.Background(), nil))
	}
	assert.Equal(t, []string{"parent", "first", "second"}, calls,
		"a derived chain must not reuse the parent chain's spare capacity: the first chain's tail was overwritten by the second chain")
}
