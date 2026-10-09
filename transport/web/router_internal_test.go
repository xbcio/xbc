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
func (nullEngine) Mount(string, string, []Handler)  {}
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

// recordingEngine remembers what each registration was handed, so a test can
// assert which engine a plane's routes reached and what chain carried them.
// The embedded nullEngine supplies the methods it does not observe.
type recordingEngine struct {
	nullEngine
	registrations []engineRegistration
}

// engineRegistration is one Handle call: the method and path the engine was
// asked to serve, and the flattened chain it was handed.
type engineRegistration struct {
	method string
	path   string
	chain  []Handler
}

func (e *recordingEngine) Handle(method, path string, chain []Handler) {
	e.registrations = append(e.registrations, engineRegistration{
		method: method,
		path:   path,
		chain:  append([]Handler(nil), chain...),
	})
}

// pathsFor reports the registrations one engine received, in order.
func (e *recordingEngine) pathsFor() []string {
	paths := make([]string, 0, len(e.registrations))
	for _, registration := range e.registrations {
		paths = append(paths, registration.method+" "+registration.path)
	}
	return paths
}

// newManagedTestRouter builds a root Router with both planes present: the
// serving plane and the management plane each write into their own recording
// engine, which is the state (*Server).Start reaches when web.management.addr
// is set. Setting the field directly is the point -- the Server's own wiring is
// asserted where the listener is bound, and these tests are about what the
// route table records for each plane.
func newManagedTestRouter(basePath string) (*Router, *recordingEngine, *recordingEngine) {
	routes, frozen, index := newRouteTable()
	serving := &recordingEngine{}
	management := &recordingEngine{}
	router := newRouter(serving, basePath, nil, routes, frozen, index)
	router.managementEngine = management
	return router, serving, management
}

// noopRoute satisfies a registration that has no behaviour to assert on.
func noopRoute(context.Context, *Ctx) error { return nil }

// TestManagementViewBindsItsRegistrationsToTheManagementEngine pins what makes
// the two planes separate at all: a route declared through Management reaches
// the management engine, and the shared route table records which plane owns
// it. A view that registered on the serving engine while marking the row as
// management -- or the reverse -- would leave the route served by one listener
// and reported as belonging to the other.
func TestManagementViewBindsItsRegistrationsToTheManagementEngine(t *testing.T) {
	router, serving, management := newManagedTestRouter("/api")

	router.GET("/orders", noopRoute)
	router.Management().GET("/metrics", noopRoute)

	assert.Equal(t, []string{"GET /api/orders"}, serving.pathsFor(),
		"a plain registration must keep reaching the serving engine")
	assert.Equal(t, []string{"GET /api/metrics"}, management.pathsFor(),
		"a registration through Management must reach the management engine, base path included")

	require.Len(t, *router.routes, 2)
	assert.False(t, (*router.routes)[0].Management)
	assert.True(t, (*router.routes)[1].Management,
		"the flag is what tells the planes apart once they share one route table")
}

// TestManagementViewKeepsTheSubtreeAndDropsTheGroupHandlers pins the two halves
// of what a group's management view inherits. The path is part of the
// registration point, so it is kept: without it a group's operator endpoint
// would silently land at the root. The handlers are not: a group's middleware
// was declared for the routes that plane serves, and the management chain runs
// none of the framework's business stages. Middleware for management routes is
// declared by grouping the view itself, which the second half of this test
// shows reaching the same engine.
func TestManagementViewKeepsTheSubtreeAndDropsTheGroupHandlers(t *testing.T) {
	router, serving, management := newManagedTestRouter("/api")
	groupMiddleware := func(context.Context, *Ctx) error { return nil }

	group := router.Group("/ops", groupMiddleware)
	group.GET("/status", noopRoute)
	group.Management().GET("/debug", noopRoute)

	require.Equal(t, []string{"GET /api/ops/status"}, serving.pathsFor())
	require.Equal(t, []string{"GET /api/ops/debug"}, management.pathsFor(),
		"a group's management view must keep the group's path")
	assert.Len(t, management.registrations[0].chain, 2,
		"a management route's chain is its route recorder and its own handler; the group's middleware was declared for the serving plane")

	view := router.Management().Group("/debug", groupMiddleware)
	view.GET("/vars", noopRoute)

	require.Equal(t, []string{"GET /api/ops/debug", "GET /api/debug/vars"}, management.pathsFor())
	assert.Len(t, management.registrations[1].chain, 3,
		"grouping the management view is how middleware reaches management routes")
	require.Len(t, *router.routes, 3)
	assert.True(t, (*router.routes)[2].Management,
		"a group derived from the management view registers on the management plane")
}

// TestManagementWithoutAListenerReturnsTheReceiver is the default-mode half of
// the seam, and the reason a plugin can call Management unconditionally: with
// no management listener configured, the route it registers is byte-for-byte
// the route it would have registered without the call -- same engine, same
// default policy, same row. The menu of a deployment that never sets
// web.management.addr must not change because a plugin started asking.
func TestManagementWithoutAListenerReturnsTheReceiver(t *testing.T) {
	router, frozen := newTestRouter("/api")

	assert.Same(t, router, router.Management(),
		"with no management listener the management view is the router itself")

	router.Perm("ops:read").Management().GET("/metrics", noopRoute)

	require.Len(t, *router.routes, 1)
	route := (*router.routes)[0]
	assert.Equal(t, "GET", route.Method)
	assert.Equal(t, "/api/metrics", route.Path)
	assert.False(t, route.Management, "an unconfigured management plane records an ordinary serving route")
	assert.Equal(t, "ops:read", route.Perm, "the group default must apply exactly as it would without the call")

	require.False(t, *frozen)
	_, err := router.freeze()
	require.NoError(t, err, "the route must freeze as the ordinary route it is")
}

// TestFreezeRejectsDeclarationsAManagementRouteCannotHonor keeps the management
// plane's policy honest. Its chain runs no authentication middleware, no
// authorization, and no in-flight gate, so a declaration made there would be
// recorded in the route table and reported to operators while nothing enforced
// it. Recording a policy that does not exist is worse than having none, so the
// freeze refuses it and names the route.
func TestFreezeRejectsDeclarationsAManagementRouteCannotHonor(t *testing.T) {
	tests := []struct {
		name    string
		declare func(*Route)
		match   string
	}{
		{"public policy", func(r *Route) { r.Auth(Public()) }, "authentication policy"},
		{"explicit policy", func(r *Route) { r.Auth(Accepts(authentication.Scheme("jwt"))) }, "authentication policy"},
		{"permission", func(r *Route) { r.Perm("ops:read") }, "permission"},
		{"unmetered", func(r *Route) { r.Unmetered() }, "unmetered"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router, _, _ := newManagedTestRouter("/")
			test.declare(router.Management().GET("/metrics", noopRoute))

			_, err := router.freeze()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "GET /metrics")
			assert.Contains(t, err.Error(), test.match)
		})
	}

	// Positive control: the same registration point freezes when it declares
	// nothing, so the rule above rejects the declarations rather than refusing
	// management routes outright.
	router, _, _ := newManagedTestRouter("/")
	router.Management().GET("/metrics", noopRoute)
	_, err := router.freeze()
	require.NoError(t, err)
}

// TestFreezeRejectsOnePathRegisteredOnBothPlanes covers the collision the two
// engines cannot see: each engine accepts the duplicate on its own, and the
// route table then holds one entry per method and path for two registrations --
// so the request-time lookup every route bakes in and the in-flight gate's
// exemption would both resolve against whichever plane registered last.
func TestFreezeRejectsOnePathRegisteredOnBothPlanes(t *testing.T) {
	router, _, _ := newManagedTestRouter("/")
	router.GET("/metrics", noopRoute)
	router.Management().GET("/metrics", noopRoute)

	_, err := router.freeze()
	require.Error(t, err, "the same method and path on both planes must not freeze")
	assert.Contains(t, err.Error(), "GET /metrics")
	assert.Contains(t, err.Error(), "management plane")

	// Positive control: two planes with their own paths freeze, so the rule is
	// the shared method and path rather than the two planes existing at once.
	clean, _, _ := newManagedTestRouter("/")
	clean.GET("/orders", noopRoute)
	clean.Management().GET("/metrics", noopRoute)
	_, err = clean.freeze()
	require.NoError(t, err)
}
