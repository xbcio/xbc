package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// Registration methods must take the engine-neutral Handler. Go function
// types are invariant in their parameters, so these assignments are the only
// construct that actually pins the parameter type -- a runtime check on a
// returned value cannot tell Handler from an engine's own handler type once
// the value is already built. If a later change widens any of these back to an
// engine-specific handler, this file stops compiling.
var (
	_ func(*web.Router, string, string, ...web.Handler) *web.Route   = (*web.Router).Handle
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).GET
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).POST
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).PUT
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).DELETE
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).PATCH
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).HEAD
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).OPTIONS
	_ func(*web.Router, string, ...web.Handler) *web.Route           = (*web.Router).Any
	_ func(*web.Router, []string, string, ...web.Handler) *web.Route = (*web.Router).Match
	_ func(*web.Router, string, ...web.Handler) *web.Router          = (*web.Router).Group
)

// newTestEngineAndRouter builds the neutral test engine and a root Router over
// it. Router.Handle bakes a recordCurrentRoute closure into the front of every
// route's own flattened chain (see Router.Handle and recordCurrentRoute's doc
// comments) -- exactly the wiring (*Server).Start relies on in production -- so
// any test built on this helper can call CurrentRoute after freeze without
// repeating that wiring itself.
//
// Route patterns here are http.ServeMux patterns ("/users/{id}"), which is what
// the neutral engine routes with; see the enginetest package documentation.
func newTestEngineAndRouter(basePath string) (*enginetest.Engine, *web.Router) {
	engine := enginetest.New()
	routes, frozen, index := web.NewRouteTable()
	return engine, web.NewRouter(engine, basePath, nil, routes, frozen, index)
}

func TestRouteMetadataChainIsFrozenIntoCatalogAndCurrentRoute(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var current web.RouteInfo
	router.Group("/users").POST("", func(_ context.Context, c *web.Ctx) error {
		current, _ = web.CurrentRoute(c)
		return nil
	}).Name("create user").Perm("user:write").Idempotent()
	router.POST("/login", func(context.Context, *web.Ctx) error { return nil }).Name("login").Auth(web.Public())
	catalog, err := router.Freeze()
	require.NoError(t, err)

	create, ok := catalog.Lookup(http.MethodPost, "/api/users")
	require.True(t, ok)
	assert.Equal(t, web.RouteInfo{
		Method:     http.MethodPost,
		Path:       "/api/users",
		Name:       "create user",
		Perm:       "user:write",
		Idempotent: true,
	}, create)

	login, ok := catalog.Lookup(http.MethodPost, "/api/login")
	require.True(t, ok)
	assert.Equal(t, "login", login.Name)
	require.NotNil(t, login.Auth)
	assert.True(t, login.Auth.IsPublic())

	req := httptest.NewRequest(http.MethodPost, "/api/users", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.Equal(t, create, current, "request-time lookup must expose the final frozen metadata")
}

// TestMethodHelpersRecordAndDispatchTheirOwnVerb pins each single-method
// helper to one HTTP verb, from both ends. The route-table assertion alone
// would pass on a helper that records the right verb but forwards a different
// one to the engine, and the dispatch assertion alone would pass on a helper
// that routes correctly but never writes a RouteInfo row -- which is the silent
// policy blind spot the route table exists to prevent, since an unrecorded
// route is invisible to authentication and authorization.
//
// The neutral engine answers a verb mismatch with 405 rather than 404 (it
// reports method-not-allowed by default), so a helper wired to the wrong verb
// is distinguishable from one that registered no path at all.
func TestMethodHelpersRecordAndDispatchTheirOwnVerb(t *testing.T) {
	helpers := map[string]func(*web.Router, string, ...web.Handler) *web.Route{
		http.MethodGet:     (*web.Router).GET,
		http.MethodPost:    (*web.Router).POST,
		http.MethodPut:     (*web.Router).PUT,
		http.MethodDelete:  (*web.Router).DELETE,
		http.MethodPatch:   (*web.Router).PATCH,
		http.MethodHead:    (*web.Router).HEAD,
		http.MethodOptions: (*web.Router).OPTIONS,
	}

	for method, register := range helpers {
		t.Run(method, func(t *testing.T) {
			engine, router := newTestEngineAndRouter("/api")
			register(router, "/thing", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusTeapot); return nil }).Perm("thing:use")

			catalog, err := router.Freeze()
			require.NoError(t, err)
			info, ok := catalog.Lookup(method, "/api/thing")
			require.Truef(t, ok, "%s /api/thing was never recorded in the route table", method)
			assert.Equal(t, "thing:use", info.Perm)

			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(method, "/api/thing", nil))
			assert.Equal(t, http.StatusTeapot, recorder.Code, "%s /api/thing did not reach the handler", method)
		})
	}
}

// TestAnyRegistersEveryMethodUnderOneMetadataHandle covers the property that
// makes a multi-method handle worth having: one .Perm call must reach every
// row the registration created. A handle that only updated the last row would
// leave the other six unguarded while looking correctly declared at the call
// site.
func TestAnyRegistersEveryMethodUnderOneMetadataHandle(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	router.Any("/resource", func(context.Context, *web.Ctx) error { return nil }).Perm("resource:use").Auth(web.Public())

	catalog, err := router.Freeze()
	require.NoError(t, err)
	for _, method := range web.AnyMethods {
		info, ok := catalog.Lookup(method, "/api/resource")
		require.Truef(t, ok, "Any did not register %s", method)
		assert.Equalf(t, "resource:use", info.Perm, "Any's handle did not carry the permission to %s", method)
		require.NotNilf(t, info.Auth, "Any's handle did not carry the policy to %s", method)
		assert.True(t, info.Auth.IsPublic())
	}
	assert.Len(t, catalog.All(), len(web.AnyMethods), "Any registered a method outside anyMethods")

	// CONNECT and TRACE are gin's Any set but deliberately not ours, so their
	// absence is a decision worth failing on rather than an accident.
	for _, method := range []string{http.MethodConnect, http.MethodTrace} {
		_, ok := catalog.Lookup(method, "/api/resource")
		assert.Falsef(t, ok, "Any must not expose %s", method)
	}
}

func TestMatchRegistersOnlyTheListedMethods(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	router.Match([]string{http.MethodGet, http.MethodHead}, "/report", func(context.Context, *web.Ctx) error { return nil }).Name("read report")

	catalog, err := router.Freeze()
	require.NoError(t, err)
	require.Len(t, catalog.All(), 2)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		info, ok := catalog.Lookup(method, "/api/report")
		require.True(t, ok)
		assert.Equal(t, "read report", info.Name)
	}
	_, ok := catalog.Lookup(http.MethodPost, "/api/report")
	assert.False(t, ok, "Match registered a method that was never listed")
}

// TestMatchRejectsAnEmptyMethodList pins the panic rather than a silent
// no-op: an empty list would register nothing while still returning a handle
// whose .Perm appears to declare a policy.
func TestMatchRejectsAnEmptyMethodList(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	assert.PanicsWithValue(t,
		"xbc: Match requires at least one HTTP method",
		func() { router.Match(nil, "/resource", func(context.Context, *web.Ctx) error { return nil }) },
	)
}

func TestRouteMetadataCannotChangeAfterFreeze(t *testing.T) {
	_, router := newTestEngineAndRouter("/")
	route := router.GET("/users", func(context.Context, *web.Ctx) error { return nil })
	_, err := router.Freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata",
		func() { route.Auth(web.Public()) },
	)
}

func TestRouteCatalogAllReturnsDefensiveCopy(t *testing.T) {
	_, router := newTestEngineAndRouter("/")
	router.GET("/a", func(context.Context, *web.Ctx) error { return nil })
	router.GET("/b", func(context.Context, *web.Ctx) error { return nil })
	catalog, err := router.Freeze()
	require.NoError(t, err)

	got := catalog.All()
	require.Len(t, got, 2)
	got[0].Path = "/mutated"

	again := catalog.All()
	assert.NotEqual(t, "/mutated", again[0].Path, "The slice received by caller must not be modified, should not affect catalog itself or subsequent All() calls")
}

func TestRouteCatalogLookupHitAndMiss(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	router.GET("/users", func(context.Context, *web.Ctx) error { return nil })
	catalog, err := router.Freeze()
	require.NoError(t, err)

	info, ok := catalog.Lookup(http.MethodGet, "/api/users")
	require.True(t, ok, "Registered method+path combination must match")
	assert.Equal(t, web.RouteInfo{Method: http.MethodGet, Path: "/api/users"}, info)

	_, ok = catalog.Lookup(http.MethodPost, "/api/users")
	assert.False(t, ok, "Different methods, even with same path, cannot match")

	_, ok = catalog.Lookup(http.MethodGet, "/api/orders")
	assert.False(t, ok, "Unregistered paths must not match")
}

func TestCurrentRouteReportsMatchedRouteDuringRequest(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var got web.RouteInfo
	var ok bool
	router.GET("/users/{id}", func(_ context.Context, c *web.Ctx) error {
		got, ok = web.CurrentRoute(c)
		return nil
	})
	_, err := router.Freeze()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.True(t, ok, "Matching requests must be able to get CurrentRoute")
	assert.Equal(t, web.RouteInfo{Method: http.MethodGet, Path: "/api/users/{id}"}, got,
		"CurrentRoute must report the original Path with path parameter placeholders from the frozen route table, not the actual value from the request")
}

func TestCurrentRouteReportsFalseForNonMatchingRequest(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")
	router.GET("/users", func(context.Context, *web.Ctx) error { return nil })
	_, err := router.Freeze()
	require.NoError(t, err)

	var ok bool
	engine.NoRoute([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		_, ok = web.CurrentRoute(c)
		return nil
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ok, "No request matched any frozen route, CurrentRoute must return false")
}

// TestRecordCurrentRouteLeadsTheFullyFlattenedChain pins Step E's position
// requirement: recordCurrentRoute must lead the entire flattened chain the
// engine dispatches, ahead of global middleware registered on the Router --
// not merely ahead of the route's own handlers passed to Handle. A wrong
// implementation that spliced recordCurrentRoute between the Router's
// accumulated middleware and the route-specific handlers would still make a
// weaker assertion pass -- "the route handler itself can see CurrentRoute" --
// because by the time a route handler runs, every earlier handler including a
// misplaced recordCurrentRoute has already executed. The only point in time
// that actually distinguishes the two placements is whether global middleware
// can already see CurrentRoute before it calls Next: with recordCurrentRoute
// correctly leading, that global middleware's own frame runs strictly after
// recordCurrentRoute's, whether or not it has invoked Next yet; with
// recordCurrentRoute misplaced, that middleware's frame runs first, so
// checking before Next observes a miss.
func TestRecordCurrentRouteLeadsTheFullyFlattenedChain(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var sawRouteBeforeNext, sawRouteAfterNext bool
	router.AppendGlobalHandler(func(_ context.Context, c *web.Ctx) error {
		_, sawRouteBeforeNext = web.CurrentRoute(c)
		c.Next()
		_, sawRouteAfterNext = web.CurrentRoute(c)
		return nil
	})
	router.GET("/widgets", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusNoContent)
		return nil
	})

	_, err := router.Freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/widgets", nil))

	assert.True(t, sawRouteBeforeNext,
		"global middleware registered on the Router before the route must see CurrentRoute even before it calls Next -- the discriminating check for recordCurrentRoute's position")
	assert.True(t, sawRouteAfterNext, "global middleware must also see CurrentRoute after Next returns")
}

// TestGroupSnapshotsParentChainAtCreationTime replaces
// TestGroupMustBeCreatedAfterUseOrMiddlewareSilentlyNeverApplies and
// TestGroupCreatedBeforeUseNeverSeesLaterMiddleware now that Engine (design
// §4.2) has no Group or Use of its own: Router.Group always takes an
// immediate, freshly allocated snapshot of the parent's chain (see
// appendChain), so there is no "Group must come after Use" ordering hazard
// left to pin here -- xbc flattens the whole chain before it ever reaches the
// engine. What still matters, and is worth pinning directly, is the snapshot
// property itself: a handler appended to the parent's chain after a child
// Group already exists must never reach that child, while every handler
// already on the parent at the moment Group was called must.
func TestGroupSnapshotsParentChainAtCreationTime(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var calls []string
	router.AppendGlobalHandler(func(context.Context, *web.Ctx) error {
		calls = append(calls, "before")
		return nil
	})

	child := router.Group("/child")

	router.AppendGlobalHandler(func(context.Context, *web.Ctx) error {
		calls = append(calls, "after")
		return nil
	})
	child.GET("/x", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil })

	_, err := router.Freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/child/x", nil))

	require.Equal(t, http.StatusNoContent, recorder.Code, "the route itself must still be reachable")
	assert.Equal(t, []string{"before"}, calls,
		"a Group must inherit exactly the parent chain as of its own creation: everything already on the parent, nothing appended to the parent afterward")
}

// TestGroupMiddlewareIsScopedToItsSubtree covers the three properties that
// make group middleware usable as an authorization boundary: it runs for the
// group's own routes, it is inherited by nested groups, and -- the one that
// actually matters for security -- it does not run for siblings registered on
// the parent. A test that only asserted the first would pass on a middleware
// mistakenly installed engine-wide, which is the failure that silently widens
// an authorization scope instead of narrowing it.
func TestGroupMiddlewareIsScopedToItsSubtree(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var calls []string
	mark := func(name string) web.Handler {
		return func(context.Context, *web.Ctx) error { calls = append(calls, name); return nil }
	}
	noop := func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil }

	admin := router.Group("/admin", mark("admin"))
	admin.GET("/users", noop)
	admin.Group("/audit", mark("audit")).GET("/entries", noop)
	router.GET("/public", noop)

	_, err := router.Freeze()
	require.NoError(t, err)

	for _, tt := range []struct {
		path string
		want []string
	}{
		{path: "/api/admin/users", want: []string{"admin"}},
		{path: "/api/admin/audit/entries", want: []string{"admin", "audit"}},
		{path: "/api/public", want: nil},
	} {
		t.Run(tt.path, func(t *testing.T) {
			calls = nil
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			require.Equal(t, http.StatusNoContent, recorder.Code, "the route itself must still be reachable")
			assert.Equal(t, tt.want, calls)
		})
	}
}

// TestGroupPermDefaultAppliesToEveryRouteInGroup pins that a group-level
// default reaches every route registered under it, not merely the first or
// last one -- Handle must read the default on every call, not cache it once.
func TestGroupPermDefaultAppliesToEveryRouteInGroup(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	g.GET("/list", func(context.Context, *web.Ctx) error { return nil })
	g.GET("/detail", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "A group-level default must reach the first route registered under the group")

	detail, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/detail")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", detail.Perm, "A group-level default must reach every route registered under the group, not only the first one")
}

// TestRoutePermOverridesGroupDefaultForSingleRoute pins that Route.Perm, run
// after Handle already wrote the group default, wins for that one route while
// leaving the group default intact for its siblings.
func TestRoutePermOverridesGroupDefaultForSingleRoute(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	g.GET("/list", func(context.Context, *web.Ctx) error { return nil })
	g.POST("/fix", func(context.Context, *web.Ctx) error { return nil }).Perm("FP_ROLE_ADMIN")

	catalog, err := router.Freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "A route that does not override it explicitly must keep the group-level default")

	fix, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/fix")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE_ADMIN", fix.Perm, "a per-route .Perm must override the group-level default")
}

// TestNestedGroupInheritsParentPermDefault pins inheritance across Group
// boundaries: a sub-group derived from a group that already declared a
// default must carry that default into its own routes.
func TestNestedGroupInheritsParentPermDefault(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	sub := g.Group("/audit")
	sub.GET("/entries", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	entries, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/audit/entries")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", entries.Perm, "A nested sub-group must inherit its parent group's group-level default")
}

// TestChildGroupPermOverrideDoesNotAffectParentOrSiblings is the
// security-critical case: a child group that overrides the inherited default
// with its own must not leak that override back into the parent group or any
// sibling registered on the parent. A shared-pointer implementation of the
// default field would fail this test even though the three tests above would
// still pass.
func TestChildGroupPermOverrideDoesNotAffectParentOrSiblings(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	child := g.Group("/admin").Perm("FP_ROLE_ADMIN")
	child.GET("/panel", func(context.Context, *web.Ctx) error { return nil })
	// Registered on g after child's override -- must still see only g's own
	// default, never child's.
	g.GET("/list", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	panel, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/admin/panel")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE_ADMIN", panel.Perm, "a child group's own override must take effect on its own routes")

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "A child group's override must never leak back into the parent group or a sibling route")
}

// TestPermDefaultSnapshotExcludesRoutesRegisteredBeforeCall pins the
// registration-time snapshot semantics documented on Router.Perm: a route
// already registered before .Perm is called keeps whatever default (or
// absence of one) existed at its own registration time.
func TestPermDefaultSnapshotExcludesRoutesRegisteredBeforeCall(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/list", func(context.Context, *web.Ctx) error { return nil })
	g.Perm("FP_ROLE")
	g.GET("/detail", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Empty(t, list.Perm, "a route registered before the .Perm call must not pick up a default that was only declared afterwards")

	detail, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/detail")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", detail.Perm, "A route registered after the .Perm call must carry the default")
}

// TestGroupDerivedBeforePermDoesNotInheritLaterDefault is the Group-boundary
// counterpart of the snapshot test above: a sub-group derived before .Perm is
// called on the parent must not retroactively pick up a default declared
// afterward, while one derived afterward must.
func TestGroupDerivedBeforePermDoesNotInheritLaterDefault(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	before := g.Group("/before")
	g.Perm("FP_ROLE")
	after := g.Group("/after")

	before.GET("/x", func(context.Context, *web.Ctx) error { return nil })
	after.GET("/y", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	x, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/before/x")
	require.True(t, ok)
	assert.Empty(t, x.Perm, "a sub-group derived before the .Perm call must not inherit a default that was only declared afterwards")

	y, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/after/y")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", y.Perm, "a sub-group derived after the .Perm call must inherit the default")
}

// TestGroupMiddlewareRunsBeforeTheHandler pins the ordering an authorization
// guard depends on. Without it a middleware could still be recorded as having
// run while the handler had already produced its response.
func TestGroupMiddlewareRunsBeforeTheHandler(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var order []string
	router.Group("/admin", func(_ context.Context, c *web.Ctx) error {
		order = append(order, "middleware")
		c.Status(http.StatusForbidden)
		c.Abort()
		return nil
	}).GET("/users", func(_ context.Context, c *web.Ctx) error {
		order = append(order, "handler")
		c.Status(http.StatusNoContent)
		return nil
	})
	_, err := router.Freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Equal(t, []string{"middleware"}, order, "an aborting group middleware must keep the handler from running")
}

// TestCascadedRegistrationRecordsBothRoutesUnderGroupPrefix pins the basic
// promise Route's embedded *Router restores: cascaded registration
// (g.GET(...).POST(...)) must actually record both routes in the table, each
// under the group's own path prefix, not just compile.
func TestCascadedRegistrationRecordsBothRoutesUnderGroupPrefix(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/a", func(context.Context, *web.Ctx) error { return nil }).POST("/b", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	_, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	assert.True(t, ok, "the first route of a cascaded registration must really enter the route table")

	_, ok = catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	assert.True(t, ok, "the second route of a cascaded registration must really enter the route table too, under the group's own path prefix")
}

// TestCascadedRegistrationStaysOnSameGroupNotRoot proves the cascaded second
// route is registered on the same group as the first, not on the root
// router: it installs group-scoped middleware and dispatches an HTTP request
// to the cascaded route, so a Route whose embedded *Router silently pointed
// back at the root would fail this test even though the route table alone
// would look identical.
func TestCascadedRegistrationStaysOnSameGroupNotRoot(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")
	var ran bool
	admin := router.Group("/admin", func(context.Context, *web.Ctx) error { ran = true; return nil })
	admin.GET("/a", func(context.Context, *web.Ctx) error { return nil }).POST("/b", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil })

	_, err := router.Freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/b", nil))
	assert.Equal(t, http.StatusNoContent, recorder.Code)
	assert.True(t, ran, "the cascaded second route must be registered on the same group, not on the root router -- registered on the root, the group's middleware never runs")
}

// TestCascadedPermOnlyAffectsLastRegisteredRoute is the load-bearing case:
// g.GET("/a", h).POST("/b", h).Perm("Y") must only mark /b, because .POST
// returns a brand-new *Route whose indexes cover only the row it just
// created. A future "fix" that made the whole chain share one *Route (and
// therefore one indexes slice) would silently widen .Perm to every route in
// the chain -- exactly the policy-misassignment this test exists to catch.
func TestCascadedPermOnlyAffectsLastRegisteredRoute(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/a", func(context.Context, *web.Ctx) error { return nil }).POST("/b", func(context.Context, *web.Ctx) error { return nil }).Perm("Y")

	catalog, err := router.Freeze()
	require.NoError(t, err)

	a, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	require.True(t, ok)
	assert.Empty(t, a.Perm, "On a cascaded chain, .Perm must reach only the *Route that returned it (/b) -- /a must never be affected")

	b, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	require.True(t, ok)
	assert.Equal(t, "Y", b.Perm, ".Perm called on the new *Route that .POST returned must land on /b")
}

// TestCascadedRegistrationInheritsGroupDefaults pins that routes created
// through cascading still go through the normal Handle path, so they pick up
// the group's registration-time Perm/Auth defaults exactly like any
// non-cascaded registration would.
func TestCascadedRegistrationInheritsGroupDefaults(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE").Auth(web.Public())
	g.GET("/a", func(context.Context, *web.Ctx) error { return nil }).POST("/b", func(context.Context, *web.Ctx) error { return nil })

	catalog, err := router.Freeze()
	require.NoError(t, err)

	a, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", a.Perm, "the first route of a cascaded registration must inherit the group-level Perm default")
	require.NotNil(t, a.Auth, "the first route of a cascaded registration must inherit the group-level Auth default")
	assert.True(t, a.Auth.IsPublic(), "the first route of a cascaded registration must inherit the group-level Auth default")

	b, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", b.Perm, "a route created by cascading must inherit the group-level Perm default just the same")
	require.NotNil(t, b.Auth, "a route created by cascading must inherit the group-level Auth default just the same")
	assert.True(t, b.Auth.IsPublic(), "a route created by cascading must inherit the group-level Auth default just the same")
}

// TestRoutePermShadowsRouterPermForGroupVsRouteScope contrasts the two .Perm
// methods side by side on the same *Router variable: called while the
// variable's static type is *Router, .Perm sets the group-level default that
// reaches every route registered afterward; called on the *Route a
// registration returns, .Perm shadows that method and only ever reaches the
// handful of rows its own indexes cover.
func TestRoutePermShadowsRouterPermForGroupVsRouteScope(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/first", func(context.Context, *web.Ctx) error { return nil })
	g.Perm("GROUP_DEFAULT") // *Router.Perm: group-level, affects registrations from here on.
	g.GET("/second", func(context.Context, *web.Ctx) error { return nil })
	g.GET("/third", func(context.Context, *web.Ctx) error { return nil }).Perm("ROUTE_ONLY") // *Route.Perm: route-level, affects only /third.

	catalog, err := router.Freeze()
	require.NoError(t, err)

	first, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/first")
	require.True(t, ok)
	assert.Empty(t, first.Perm, "a route registered before the group-level .Perm call must not be affected by it")

	second, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/second")
	require.True(t, ok)
	assert.Equal(t, "GROUP_DEFAULT", second.Perm, ".Perm on a *Router is the group-level default, so it must reach every route registered after it")

	third, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/third")
	require.True(t, ok)
	assert.Equal(t, "ROUTE_ONLY", third.Perm, ".Perm on a *Route shadows the *Router method of the same name: it applies to this one route only, and it replaces the group-level default rather than merging with it")
}

// TestPromotedRegistrationMethodPanicsAfterFreeze pins that freezing the
// route table still blocks new registrations reached through a cascaded,
// promoted method call -- not just ones reached directly on a *Router. The
// panic must come from the same guard Handle already has, regardless of
// which handle the caller went through to reach it.
func TestPromotedRegistrationMethodPanicsAfterFreeze(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	route := router.GET("/a", func(context.Context, *web.Ctx) error { return nil })
	_, err := router.Freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot add routes",
		func() { route.POST("/b", func(context.Context, *web.Ctx) error { return nil }) },
		"a registration method reached through a promoted cascaded handle must panic after freeze just the same -- a different call path must not bypass the freeze check",
	)
}

// TestCascadeAfterAnyOrMatchOwnsOnlyItsOwnIndexes covers cascading off the
// handle Any/Match return: that handle's indexes already cover multiple
// rows, so the cascaded call's own *Route must still carry only the row(s)
// it itself created, never inheriting or extending the multi-row indexes of
// the handle it was chained from.
func TestCascadeAfterAnyOrMatchOwnsOnlyItsOwnIndexes(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	router.Match([]string{http.MethodGet, http.MethodHead}, "/report", func(context.Context, *web.Ctx) error { return nil }).
		POST("/create", func(context.Context, *web.Ctx) error { return nil }).Perm("CREATE_ONLY")

	catalog, err := router.Freeze()
	require.NoError(t, err)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		info, ok := catalog.Lookup(method, "/api/report")
		require.Truef(t, ok, "%s /api/report must still be registered", method)
		assert.Emptyf(t, info.Perm, "a route created by Match must not be touched by a .Perm cascaded off its handle afterwards: %s", method)
	}

	create, ok := catalog.Lookup(http.MethodPost, "/api/create")
	require.True(t, ok)
	assert.Equal(t, "CREATE_ONLY", create.Perm, "A .POST cascaded off a Match handle must carry indexes of its own, so .Perm lands only on the row that call itself created")
}

// TestSiblingGroupsDoNotShareMiddlewareChain pins what sibling groups must
// observe end to end: each one inherits the whole parent chain, in the order
// it was declared, followed by its own handler -- and neither sibling ever
// sees the other's. It covers inheritance and execution order through a real
// request, which is the property a route tree is actually read for.
//
// It does not, and cannot, guard the aliasing hazard behind appendChain. Every
// chain on this path is normalised by appendChain itself (newRouter does it
// for the root, Group for each child), so under a correct implementation cap
// always equals len here and the input that makes a naive append reuse its
// parent's backing array is unreachable -- whether the corruption shows up at
// all would depend on how Go's allocator happens to round a slice's size.
// TestAppendChainNeverWritesIntoParentSpareCapacity (router_internal_test.go)
// builds that input directly and guards the hazard there.
//
// Both groups are deliberately created before either registers a route. The
// engine copies the chain synchronously at registration, so the interleaved
// order -- create /a, register /a, create /b -- would let /a snapshot its
// chain before /b could disturb it. Declaring both groups first is what a real
// route tree does anyway.
func TestSiblingGroupsDoNotShareMiddlewareChain(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var calls []string
	mark := func(name string) web.Handler {
		return func(context.Context, *web.Ctx) error { calls = append(calls, name); return nil }
	}
	noop := func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil }

	admin := router.Group("/admin",
		mark("p1"), mark("p2"), mark("p3"), mark("p4"), mark("p5"))
	first := admin.Group("/a", mark("a"))
	second := admin.Group("/b", mark("b"))
	first.GET("/x", noop)
	second.GET("/x", noop)

	_, err := router.Freeze()
	require.NoError(t, err)

	for _, tt := range []struct {
		path string
		want []string
	}{
		{path: "/api/admin/a/x", want: []string{"p1", "p2", "p3", "p4", "p5", "a"}},
		{path: "/api/admin/b/x", want: []string{"p1", "p2", "p3", "p4", "p5", "b"}},
	} {
		t.Run(tt.path, func(t *testing.T) {
			calls = nil
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			require.Equal(t, http.StatusNoContent, recorder.Code, "the route itself must still be reachable")
			assert.Equal(t, tt.want, calls, "sibling groups must not share or overwrite each other's middleware chain")
		})
	}
}
