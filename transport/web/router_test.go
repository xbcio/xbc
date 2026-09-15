package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/extensions/authentication"
)

// Registration methods must take the engine-neutral Handler. Go function
// types are invariant in their parameters, so these assignments are the only
// construct that actually pins the parameter type -- a runtime check on a
// returned value cannot tell Handler from gin.HandlerFunc once the value is
// already built. If a later change widens any of these back to
// gin.HandlerFunc, this file stops compiling.
var (
	_ func(*Router, string, string, ...Handler) *Route   = (*Router).Handle
	_ func(*Router, string, ...Handler) *Route           = (*Router).GET
	_ func(*Router, string, ...Handler) *Route           = (*Router).POST
	_ func(*Router, string, ...Handler) *Route           = (*Router).PUT
	_ func(*Router, string, ...Handler) *Route           = (*Router).DELETE
	_ func(*Router, string, ...Handler) *Route           = (*Router).PATCH
	_ func(*Router, string, ...Handler) *Route           = (*Router).HEAD
	_ func(*Router, string, ...Handler) *Route           = (*Router).OPTIONS
	_ func(*Router, string, ...Handler) *Route           = (*Router).Any
	_ func(*Router, []string, string, ...Handler) *Route = (*Router).Match
	_ func(*Router, string, ...Handler) *Router          = (*Router).Group
)

// newTestEngineAndRouter builds a testEngine (this package's stand-in for
// engines/gin's real adapter, see enginetest_test.go) and a root Router over
// it, with recordCurrentRoute already installed as the first handler in the
// chain -- exactly the order (*Server).Start uses in production, so any test
// built on this helper can call CurrentRoute after freeze without repeating
// that wiring itself.
func newTestEngineAndRouter(basePath string) (*testEngine, *Router, *bool, *map[string]RouteInfo) {
	engine := newTestEngine()
	routes, frozen, index := newRouteTable()
	router := newRouter(engine, basePath, []Handler{recordCurrentRoute(frozen, index)}, routes, frozen, index)
	return engine, router, frozen, index
}

func TestRouteMetadataChainIsFrozenIntoCatalogAndCurrentRoute(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var current RouteInfo
	router.Group("/users").POST("", func(_ context.Context, c *Ctx) error {
		current, _ = CurrentRoute(c)
		return nil
	}).Name("create user").Perm("user:write").Idempotent()
	router.POST("/login", func(context.Context, *Ctx) error { return nil }).Name("login").Auth(Public())
	catalog, err := router.freeze()
	require.NoError(t, err)

	create, ok := catalog.Lookup(http.MethodPost, "/api/users")
	require.True(t, ok)
	assert.Equal(t, RouteInfo{
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
// one to gin, and the dispatch assertion alone would pass on a helper that
// routes correctly but never writes a RouteInfo row -- which is the silent
// policy blind spot the route table exists to prevent, since an unrecorded
// route is invisible to authentication and authorization.
func TestMethodHelpersRecordAndDispatchTheirOwnVerb(t *testing.T) {
	helpers := map[string]func(*Router, string, ...Handler) *Route{
		http.MethodGet:     (*Router).GET,
		http.MethodPost:    (*Router).POST,
		http.MethodPut:     (*Router).PUT,
		http.MethodDelete:  (*Router).DELETE,
		http.MethodPatch:   (*Router).PATCH,
		http.MethodHead:    (*Router).HEAD,
		http.MethodOptions: (*Router).OPTIONS,
	}

	for method, register := range helpers {
		t.Run(method, func(t *testing.T) {
			engine, router, _, _ := newTestEngineAndRouter("/api")
			// HandleMethodNotAllowed makes a verb mismatch a 405 rather than a
			// 404, so a helper wired to the wrong verb is distinguishable from
			// one that registered no path at all.
			engine.gin.HandleMethodNotAllowed = true
			register(router, "/thing", func(_ context.Context, c *Ctx) error { c.Status(http.StatusTeapot); return nil }).Perm("thing:use")

			catalog, err := router.freeze()
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
	_, router, _, _ := newTestEngineAndRouter("/api")
	router.Any("/resource", func(context.Context, *Ctx) error { return nil }).Perm("resource:use").Auth(Public())

	catalog, err := router.freeze()
	require.NoError(t, err)
	for _, method := range anyMethods {
		info, ok := catalog.Lookup(method, "/api/resource")
		require.Truef(t, ok, "Any did not register %s", method)
		assert.Equalf(t, "resource:use", info.Perm, "Any's handle did not carry the permission to %s", method)
		require.NotNilf(t, info.Auth, "Any's handle did not carry the policy to %s", method)
		assert.True(t, info.Auth.IsPublic())
	}
	assert.Len(t, catalog.All(), len(anyMethods), "Any registered a method outside anyMethods")

	// CONNECT and TRACE are gin's Any set but deliberately not ours, so their
	// absence is a decision worth failing on rather than an accident.
	for _, method := range []string{http.MethodConnect, http.MethodTrace} {
		_, ok := catalog.Lookup(method, "/api/resource")
		assert.Falsef(t, ok, "Any must not expose %s", method)
	}
}

func TestMatchRegistersOnlyTheListedMethods(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	router.Match([]string{http.MethodGet, http.MethodHead}, "/report", func(context.Context, *Ctx) error { return nil }).Name("read report")

	catalog, err := router.freeze()
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
	_, router, _, _ := newTestEngineAndRouter("/api")
	assert.PanicsWithValue(t,
		"xbc: Match requires at least one HTTP method",
		func() { router.Match(nil, "/resource", func(context.Context, *Ctx) error { return nil }) },
	)
}

func TestRouteMetadataCannotChangeAfterFreeze(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/")
	route := router.GET("/users", func(context.Context, *Ctx) error { return nil })
	_, err := router.freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata",
		func() { route.Auth(Public()) },
	)
}

func TestRouteCatalogAllReturnsDefensiveCopy(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/")
	router.GET("/a", func(context.Context, *Ctx) error { return nil })
	router.GET("/b", func(context.Context, *Ctx) error { return nil })
	catalog, err := router.freeze()
	require.NoError(t, err)

	got := catalog.All()
	require.Len(t, got, 2)
	got[0].Path = "/mutated"

	again := catalog.All()
	assert.NotEqual(t, "/mutated", again[0].Path, "The slice received by caller must not be modified, should not affect catalog itself or subsequent All() calls")
}

func TestRouteCatalogLookupHitAndMiss(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	router.GET("/users", func(context.Context, *Ctx) error { return nil })
	catalog, err := router.freeze()
	require.NoError(t, err)

	info, ok := catalog.Lookup(http.MethodGet, "/api/users")
	require.True(t, ok, "Registered method+path combination must match")
	assert.Equal(t, RouteInfo{Method: http.MethodGet, Path: "/api/users"}, info)

	_, ok = catalog.Lookup(http.MethodPost, "/api/users")
	assert.False(t, ok, "Different methods, even with same path, cannot match")

	_, ok = catalog.Lookup(http.MethodGet, "/api/orders")
	assert.False(t, ok, "Unregistered paths must not match")
}

func TestCurrentRouteReportsMatchedRouteDuringRequest(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var got RouteInfo
	var ok bool
	router.GET("/users/:id", func(_ context.Context, c *Ctx) error {
		got, ok = CurrentRoute(c)
		return nil
	})
	_, err := router.freeze()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.True(t, ok, "Matching requests must be able to get CurrentRoute")
	assert.Equal(t, RouteInfo{Method: http.MethodGet, Path: "/api/users/:id"}, got,
		"CurrentRoute must report the original Path with path parameter placeholders from the frozen route table, not the actual value from the request")
}

func TestCurrentRouteReportsFalseForNonMatchingRequest(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")
	router.GET("/users", func(context.Context, *Ctx) error { return nil })
	_, err := router.freeze()
	require.NoError(t, err)

	var ok bool
	engine.NoRoute([]Handler{func(_ context.Context, c *Ctx) error {
		_, ok = CurrentRoute(c)
		return nil
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ok, "No request matched any frozen route, CurrentRoute must return false")
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
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var calls []string
	router.handlers = appendChain(router.handlers, func(context.Context, *Ctx) error {
		calls = append(calls, "before")
		return nil
	})

	child := router.Group("/child")

	router.handlers = appendChain(router.handlers, func(context.Context, *Ctx) error {
		calls = append(calls, "after")
		return nil
	})
	child.GET("/x", func(_ context.Context, c *Ctx) error { c.Status(http.StatusNoContent); return nil })

	_, err := router.freeze()
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
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var calls []string
	mark := func(name string) Handler {
		return func(context.Context, *Ctx) error { calls = append(calls, name); return nil }
	}
	noop := func(_ context.Context, c *Ctx) error { c.Status(http.StatusNoContent); return nil }

	admin := router.Group("/admin", mark("admin"))
	admin.GET("/users", noop)
	admin.Group("/audit", mark("audit")).GET("/entries", noop)
	router.GET("/public", noop)

	_, err := router.freeze()
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
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	g.GET("/list", func(context.Context, *Ctx) error { return nil })
	g.GET("/detail", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "组级默认必须落进该组下第一条路由")

	detail, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/detail")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", detail.Perm, "组级默认必须落进该组下每一条路由，而不仅仅是第一条")
}

// TestRoutePermOverridesGroupDefaultForSingleRoute pins that Route.Perm, run
// after Handle already wrote the group default, wins for that one route while
// leaving the group default intact for its siblings.
func TestRoutePermOverridesGroupDefaultForSingleRoute(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	g.GET("/list", func(context.Context, *Ctx) error { return nil })
	g.POST("/fix", func(context.Context, *Ctx) error { return nil }).Perm("FP_ROLE_ADMIN")

	catalog, err := router.freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "未显式覆盖的路由必须保留组级默认")

	fix, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/fix")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE_ADMIN", fix.Perm, "单条 .Perm 必须覆盖组级默认")
}

// TestNestedGroupInheritsParentPermDefault pins inheritance across Group
// boundaries: a sub-group derived from a group that already declared a
// default must carry that default into its own routes.
func TestNestedGroupInheritsParentPermDefault(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	sub := g.Group("/audit")
	sub.GET("/entries", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	entries, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/audit/entries")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", entries.Perm, "嵌套子组必须继承父组的组级默认")
}

// TestChildGroupPermOverrideDoesNotAffectParentOrSiblings is the
// security-critical case: a child group that overrides the inherited default
// with its own must not leak that override back into the parent group or any
// sibling registered on the parent. A shared-pointer implementation of the
// default field would fail this test even though the three tests above would
// still pass.
func TestChildGroupPermOverrideDoesNotAffectParentOrSiblings(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE")
	child := g.Group("/admin").Perm("FP_ROLE_ADMIN")
	child.GET("/panel", func(context.Context, *Ctx) error { return nil })
	// Registered on g after child's override -- must still see only g's own
	// default, never child's.
	g.GET("/list", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	panel, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/admin/panel")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE_ADMIN", panel.Perm, "子组的覆盖必须对自己的路由生效")

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", list.Perm, "子组覆盖默认绝不能泄漏回父组或兄弟路由")
}

// TestPermDefaultSnapshotExcludesRoutesRegisteredBeforeCall pins the
// registration-time snapshot semantics documented on Router.Perm: a route
// already registered before .Perm is called keeps whatever default (or
// absence of one) existed at its own registration time.
func TestPermDefaultSnapshotExcludesRoutesRegisteredBeforeCall(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/list", func(context.Context, *Ctx) error { return nil })
	g.Perm("FP_ROLE")
	g.GET("/detail", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	list, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/list")
	require.True(t, ok)
	assert.Empty(t, list.Perm, "在 .Perm 调用之前注册的路由不应带上之后才声明的默认值")

	detail, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/detail")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", detail.Perm, "在 .Perm 调用之后注册的路由必须带上默认值")
}

// TestGroupDerivedBeforePermDoesNotInheritLaterDefault is the Group-boundary
// counterpart of the snapshot test above: a sub-group derived before .Perm is
// called on the parent must not retroactively pick up a default declared
// afterward, while one derived afterward must.
func TestGroupDerivedBeforePermDoesNotInheritLaterDefault(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	before := g.Group("/before")
	g.Perm("FP_ROLE")
	after := g.Group("/after")

	before.GET("/x", func(context.Context, *Ctx) error { return nil })
	after.GET("/y", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	x, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/before/x")
	require.True(t, ok)
	assert.Empty(t, x.Perm, "在 .Perm 调用之前派生的子组不应继承之后声明的默认值")

	y, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/after/y")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", y.Perm, "在 .Perm 调用之后派生的子组必须继承默认值")
}

// TestGroupAuthDefaultAppliesAndIsPerRouteDeepCopy covers Router.Auth's
// group-level default and the requirement that Handle clones the policy per
// route. It inspects the mutable route table directly (this test lives in
// package web) rather than through RouteCatalog, because RouteCatalog already
// defensive-copies on every read -- that would mask a Handle that stored the
// same *AuthPolicy pointer on every route instead of cloning it.
func TestGroupAuthDefaultAppliesAndIsPerRouteDeepCopy(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")

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
	_, router, _, _ := newTestEngineAndRouter("/api")
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

// TestGroupMiddlewareRunsBeforeTheHandler pins the ordering an authorization
// guard depends on. Without it a middleware could still be recorded as having
// run while the handler had already produced its response.
func TestGroupMiddlewareRunsBeforeTheHandler(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var order []string
	router.Group("/admin", func(_ context.Context, c *Ctx) error {
		order = append(order, "middleware")
		c.Status(http.StatusForbidden)
		c.Abort()
		return nil
	}).GET("/users", func(_ context.Context, c *Ctx) error {
		order = append(order, "handler")
		c.Status(http.StatusNoContent)
		return nil
	})
	_, err := router.freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Equal(t, []string{"middleware"}, order, "an aborting group middleware must keep the handler from running")
}

// TestCascadedRegistrationRecordsBothRoutesUnderGroupPrefix pins the basic
// promise Route's embedded *Router restores: gin-style cascaded registration
// (g.GET(...).POST(...)) must actually record both routes in the table, each
// under the group's own path prefix, not just compile.
func TestCascadedRegistrationRecordsBothRoutesUnderGroupPrefix(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/a", func(context.Context, *Ctx) error { return nil }).POST("/b", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	_, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	assert.True(t, ok, "级联注册的第一条路由必须真的进入路由表")

	_, ok = catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	assert.True(t, ok, "级联注册的第二条路由必须真的进入路由表，且带有正确的组路径前缀")
}

// TestCascadedRegistrationStaysOnSameGroupNotRoot proves the cascaded second
// route is registered on the same gin group as the first, not on the root
// router: it installs group-scoped middleware and dispatches an HTTP request
// to the cascaded route, so a Route whose embedded *Router silently pointed
// back at the root would fail this test even though the route table alone
// would look identical.
func TestCascadedRegistrationStaysOnSameGroupNotRoot(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")
	var ran bool
	admin := router.Group("/admin", func(context.Context, *Ctx) error { ran = true; return nil })
	admin.GET("/a", func(context.Context, *Ctx) error { return nil }).POST("/b", func(_ context.Context, c *Ctx) error { c.Status(http.StatusNoContent); return nil })

	_, err := router.freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/b", nil))
	assert.Equal(t, http.StatusNoContent, recorder.Code)
	assert.True(t, ran, "级联注册的第二条路由必须落在同一个组上，而不是根路由器，否则组中间件不会运行")
}

// TestCascadedPermOnlyAffectsLastRegisteredRoute is the load-bearing case:
// g.GET("/a", h).POST("/b", h).Perm("Y") must only mark /b, because .POST
// returns a brand-new *Route whose indexes cover only the row it just
// created. A future "fix" that made the whole chain share one *Route (and
// therefore one indexes slice) would silently widen .Perm to every route in
// the chain -- exactly the policy-misassignment this test exists to catch.
func TestCascadedPermOnlyAffectsLastRegisteredRoute(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/a", func(context.Context, *Ctx) error { return nil }).POST("/b", func(context.Context, *Ctx) error { return nil }).Perm("Y")

	catalog, err := router.freeze()
	require.NoError(t, err)

	a, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	require.True(t, ok)
	assert.Empty(t, a.Perm, "级联链上 .Perm 只应作用于返回它的那个 *Route（/b），/a 绝不能受影响")

	b, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	require.True(t, ok)
	assert.Equal(t, "Y", b.Perm, ".POST 返回的新 *Route 上调用 .Perm 必须落在 /b 上")
}

// TestCascadedRegistrationInheritsGroupDefaults pins that routes created
// through cascading still go through the normal Handle path, so they pick up
// the group's registration-time Perm/Auth defaults exactly like any
// non-cascaded registration would.
func TestCascadedRegistrationInheritsGroupDefaults(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role").Perm("FP_ROLE").Auth(Public())
	g.GET("/a", func(context.Context, *Ctx) error { return nil }).POST("/b", func(context.Context, *Ctx) error { return nil })

	catalog, err := router.freeze()
	require.NoError(t, err)

	a, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/a")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", a.Perm, "级联注册的第一条路由必须继承组级 Perm 默认")
	require.NotNil(t, a.Auth, "级联注册的第一条路由必须继承组级 Auth 默认")
	assert.True(t, a.Auth.IsPublic(), "级联注册的第一条路由必须继承组级 Auth 默认")

	b, ok := catalog.Lookup(http.MethodPost, "/api/sys_role/b")
	require.True(t, ok)
	assert.Equal(t, "FP_ROLE", b.Perm, "级联注册出来的第二条路由同样必须继承组级 Perm 默认")
	require.NotNil(t, b.Auth, "级联注册出来的第二条路由同样必须继承组级 Auth 默认")
	assert.True(t, b.Auth.IsPublic(), "级联注册出来的第二条路由同样必须继承组级 Auth 默认")
}

// TestRoutePermShadowsRouterPermForGroupVsRouteScope contrasts the two .Perm
// methods side by side on the same *Router variable: called while the
// variable's static type is *Router, .Perm sets the group-level default that
// reaches every route registered afterward; called on the *Route a
// registration returns, .Perm shadows that method and only ever reaches the
// handful of rows its own indexes cover.
func TestRoutePermShadowsRouterPermForGroupVsRouteScope(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	g := router.Group("/sys_role")
	g.GET("/first", func(context.Context, *Ctx) error { return nil })
	g.Perm("GROUP_DEFAULT") // *Router.Perm: group-level, affects registrations from here on.
	g.GET("/second", func(context.Context, *Ctx) error { return nil })
	g.GET("/third", func(context.Context, *Ctx) error { return nil }).Perm("ROUTE_ONLY") // *Route.Perm: route-level, affects only /third.

	catalog, err := router.freeze()
	require.NoError(t, err)

	first, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/first")
	require.True(t, ok)
	assert.Empty(t, first.Perm, "组级 .Perm 调用之前注册的路由不应受影响")

	second, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/second")
	require.True(t, ok)
	assert.Equal(t, "GROUP_DEFAULT", second.Perm, "*Router 上的 .Perm 是组级默认，必须影响其后注册的路由")

	third, ok := catalog.Lookup(http.MethodGet, "/api/sys_role/third")
	require.True(t, ok)
	assert.Equal(t, "ROUTE_ONLY", third.Perm, "*Route 上的 .Perm 遮蔽了 *Router 的同名方法，只作用于这一条路由，覆盖组级默认而不是与其合并")
}

// TestPromotedRegistrationMethodPanicsAfterFreeze pins that freezing the
// route table still blocks new registrations reached through a cascaded,
// promoted method call -- not just ones reached directly on a *Router. The
// panic must come from the same guard Handle already has, regardless of
// which handle the caller went through to reach it.
func TestPromotedRegistrationMethodPanicsAfterFreeze(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	route := router.GET("/a", func(context.Context, *Ctx) error { return nil })
	_, err := router.freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot add routes",
		func() { route.POST("/b", func(context.Context, *Ctx) error { return nil }) },
		"通过级联句柄提升出来的注册方法在冻结之后调用同样必须 panic，不能因为换了调用路径就绕过 freeze 检查",
	)
}

// TestCascadeAfterAnyOrMatchOwnsOnlyItsOwnIndexes covers cascading off the
// handle Any/Match return: that handle's indexes already cover multiple
// rows, so the cascaded call's own *Route must still carry only the row(s)
// it itself created, never inheriting or extending the multi-row indexes of
// the handle it was chained from.
func TestCascadeAfterAnyOrMatchOwnsOnlyItsOwnIndexes(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	router.Match([]string{http.MethodGet, http.MethodHead}, "/report", func(context.Context, *Ctx) error { return nil }).
		POST("/create", func(context.Context, *Ctx) error { return nil }).Perm("CREATE_ONLY")

	catalog, err := router.freeze()
	require.NoError(t, err)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		info, ok := catalog.Lookup(method, "/api/report")
		require.Truef(t, ok, "%s /api/report 必须仍然注册成功", method)
		assert.Emptyf(t, info.Perm, "Match 创建的路由不应被后续级联出来的 .Perm 影响：%s", method)
	}

	create, ok := catalog.Lookup(http.MethodPost, "/api/create")
	require.True(t, ok)
	assert.Equal(t, "CREATE_ONLY", create.Perm, "Match 句柄级联出的 .POST 必须携带自己独立的 indexes，.Perm 只落在它自己创建的那一行上")
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

// TestSiblingGroupsDoNotShareMiddlewareChain guards the aliasing hazard the
// self-held handler stack introduces: append may write into the parent
// chain's spare capacity, so two sibling groups derived from the same parent
// would silently overwrite each other's middleware -- one authorization
// boundary replaced by another's, with no compile error and no panic.
//
// Two details make the bug observable, and removing either one hides it.
//
// The parent carries five handlers because a five-element slice of function
// values is 40 bytes, which append rounds up to the 48-byte size class: a
// naive append(parent, extra...) then returns cap 6 > len 5, so both siblings
// write the same backing array slot. A parent whose cap equals its len would
// force an allocation and the naive version would look correct.
//
// Both groups are also created before either registers a route. gin's
// combineHandlers copies the chain synchronously at registration, so the
// interleaved order -- create /a, register /a, create /b -- would let /a
// snapshot its chain before /b overwrote the shared slot, and the corruption
// would never reach a request. Declaring both groups first is what a real
// route tree does anyway.
func TestSiblingGroupsDoNotShareMiddlewareChain(t *testing.T) {
	engine, router, _, _ := newTestEngineAndRouter("/api")

	var calls []string
	mark := func(name string) Handler {
		return func(context.Context, *Ctx) error { calls = append(calls, name); return nil }
	}
	noop := func(_ context.Context, c *Ctx) error { c.Status(http.StatusNoContent); return nil }

	admin := router.Group("/admin",
		mark("p1"), mark("p2"), mark("p3"), mark("p4"), mark("p5"))
	first := admin.Group("/a", mark("a"))
	second := admin.Group("/b", mark("b"))
	first.GET("/x", noop)
	second.GET("/x", noop)

	_, err := router.freeze()
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
			require.Equal(t, http.StatusNoContent, recorder.Code, "路由本身必须仍然可达")
			assert.Equal(t, tt.want, calls, "兄弟分组不得共享或覆盖彼此的中间件链")
		})
	}
}
