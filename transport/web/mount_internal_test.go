package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountedHandler is the handler every Mount call in these tests registers.
// The internal tests read the route table rather than dispatch a request, so
// it only has to be non-nil.
func mountedHandler() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}

// okRouteHandler is the Handler shape the overlap tests register as the
// non-mount side of a conflict.
func okRouteHandler(context.Context, *Ctx) error { return nil }

// TestMountWritesOneMountedRowPerAnyMethod pins what a mount records: one
// RouteInfo per anyMethods verb, all naming the full prefix, all flagged
// Mounted, and all reachable from the single *Route handle the call returns.
// Mounted is load bearing beyond bookkeeping -- freeze reads it to refuse
// Unmetered -- so a mount that forgot it would look correct in the catalog
// while silently accepting a gate exemption that can never match.
func TestMountWritesOneMountedRowPerAnyMethod(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.Mount("/flow", mountedHandler()).Name("flow subtree").Perm("flow:use").Auth(Public())

	require.Len(t, *router.routes, len(anyMethods))
	for i, method := range anyMethods {
		route := (*router.routes)[i]
		assert.Equal(t, method, route.Method, "row %d", i)
		assert.Equal(t, "/api/flow", route.Path, "row %d", i)
		assert.True(t, route.Mounted, "the %s row must be marked mounted", method)
		assert.Equal(t, "flow subtree", route.Name)
		assert.Equal(t, "flow:use", route.Perm)
		require.NotNil(t, route.Auth)
		assert.True(t, route.Auth.IsPublic())
	}
	assert.NotSame(t, (*router.routes)[0].Auth, (*router.routes)[1].Auth,
		"each mounted row must carry its own Auth policy copy")

	catalog, err := router.freeze()
	require.NoError(t, err)
	info, ok := catalog.Lookup(http.MethodGet, "/api/flow")
	require.True(t, ok, "the prefix must be a literal row in the frozen catalog")
	assert.True(t, info.Mounted)
	_, ok = catalog.Lookup(http.MethodGet, "/api/flow/status")
	assert.False(t, ok, "Lookup is literal: a path inside the subtree is not a row")
}

// TestMountJoinsTheGroupPathAndInheritsGroupDefaults covers how a mount
// composes with the router it is called on: Group prefixes the path, and the
// group's Perm default reaches every row exactly as it does for a route
// registered through the same group.
func TestMountJoinsTheGroupPathAndInheritsGroupDefaults(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.Group("/tenants").Perm("tenant:admin").Mount("/flow", mountedHandler())

	require.Len(t, *router.routes, len(anyMethods))
	for _, route := range *router.routes {
		assert.Equal(t, "/api/tenants/flow", route.Path)
		assert.Equal(t, "tenant:admin", route.Perm)
		assert.True(t, route.Mounted)
	}
}

// TestMountRefusesPrefixesItCannotOwn pins the refusals a prefix can hit
// before anything is registered. Each one stands for a shape that would mean
// something the literal prefix does not: the root, which the framework's own
// routes and unmatched-request chains occupy; a trailing slash, which names
// the same subtree while putting the engine's subtree wildcard behind a
// doubled slash; and ":" or "*", which is engine routing syntax rather than a
// literal path.
func TestMountRefusesPrefixesItCannotOwn(t *testing.T) {
	const rootRefusal = "xbc: Mount cannot claim the service root, where every other route and the framework's unmatched-request chains live"

	cases := []struct {
		name      string
		basePath  string
		mount     func(*Router)
		wantPanic string
	}{
		{
			name:      "the service root",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("/", mountedHandler()) },
			wantPanic: rootRefusal,
		},
		{
			name:      "the empty path on the root router",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("", mountedHandler()) },
			wantPanic: rootRefusal,
		},
		{
			name:      "a trailing slash",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("/flow/", mountedHandler()) },
			wantPanic: `xbc: Mount requires a prefix without a trailing slash, got "/flow/"`,
		},
		{
			name:      "a group's own path, which joins with a trailing slash",
			basePath:  "/api",
			mount:     func(r *Router) { r.Group("/tenants").Mount("/", mountedHandler()) },
			wantPanic: `xbc: Mount requires a prefix without a trailing slash, got "/api/tenants/"`,
		},
		{
			name:      "a colon in the prefix",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("/fl:ow", mountedHandler()) },
			wantPanic: `xbc: Mount requires a literal prefix, but "/fl:ow" contains ":" or "*"`,
		},
		{
			name:      "a star in the prefix",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("/fl*ow", mountedHandler()) },
			wantPanic: `xbc: Mount requires a literal prefix, but "/fl*ow" contains ":" or "*"`,
		},
		{
			name:      "a nil handler",
			basePath:  "/",
			mount:     func(r *Router) { r.Mount("/flow", nil) },
			wantPanic: "xbc: Mount requires a non-nil http.Handler",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, _ := newTestRouter(tc.basePath)
			require.PanicsWithValue(t, tc.wantPanic, func() { tc.mount(router) })
			assert.Empty(t, *router.routes, "a refused mount must register nothing")
		})
	}
}

// TestMountRefusesToRegisterAfterFreeze shares Handle's refusal and its
// message: the route table is complete once RouteCatalogListener is about to
// run, so a mount arriving after freeze is the same programming mistake.
func TestMountRefusesToRegisterAfterFreeze(t *testing.T) {
	router, _ := newTestRouter("/api")
	_, err := router.freeze()
	require.NoError(t, err)

	require.PanicsWithValue(t, "xbc: route table is frozen, RouteCatalogListener phase cannot add routes",
		func() { router.Mount("/flow", mountedHandler()) })
}

// TestMountRefusesOverlappingRegistrations pins the registration-time
// conflict rule at every shape of overlap this side of the pair can take, in
// both directions, pattern routes included, plus the shapes that must stay
// legal. The check is what lets the adapters keep their own, disagreeing
// conflict behaviour out of the picture: gin panics on a conflicting subtree
// without naming what it conflicts with, and ServeMux accepts the overlap
// silently.
//
// wantRows is asserted after the call either way, so a refused registration
// is also pinned to have registered nothing.
func TestMountRefusesOverlappingRegistrations(t *testing.T) {
	cases := []struct {
		name      string
		register  func(*Router)
		wantPanic string
		wantRows  int
	}{
		{
			name: "a route at the prefix first",
			register: func(r *Router) {
				r.GET("/flow", okRouteHandler)
				r.Mount("/flow", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow overlaps route GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  1,
		},
		{
			name: "a route inside the subtree first",
			register: func(r *Router) {
				r.GET("/flow/status", okRouteHandler)
				r.Mount("/flow", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow overlaps route GET /api/flow/status registered on the same method, and a mount owns its whole subtree",
			wantRows:  1,
		},
		{
			name: "a second mount at the same prefix",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.Mount("/flow", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow overlaps mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a mount inside the mounted subtree",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.Mount("/flow/status", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow/status overlaps mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a mount swallowing an earlier mount",
			register: func(r *Router) {
				r.Mount("/flow/status", mountedHandler())
				r.Mount("/flow", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow overlaps mount GET /api/flow/status registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a route at the mount afterwards",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.GET("/flow", okRouteHandler)
			},
			wantPanic: "xbc: route GET /api/flow overlaps the mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a route under the mount afterwards",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.GET("/flow/status", okRouteHandler)
			},
			wantPanic: "xbc: route GET /api/flow/status overlaps the mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a pattern route under the mount afterwards",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.GET("/flow/:id", okRouteHandler)
			},
			wantPanic: "xbc: route GET /api/flow/:id overlaps the mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a catch-all route reaching over the mount afterwards",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.GET("/*all", okRouteHandler)
			},
			wantPanic: "xbc: route GET /api/*all overlaps the mount GET /api/flow registered on the same method, and a mount owns its whole subtree",
			wantRows:  len(anyMethods),
		},
		{
			name: "a catch-all route above the mount first",
			register: func(r *Router) {
				r.GET("/flow/*rest", okRouteHandler)
				r.Mount("/flow/status", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow/status overlaps route GET /api/flow/*rest registered on the same method, and a mount owns its whole subtree",
			wantRows:  1,
		},
		{
			name: "a parameter route above the mount first",
			register: func(r *Router) {
				r.GET("/flow/:id", okRouteHandler)
				r.Mount("/flow/status", mountedHandler())
			},
			wantPanic: "xbc: mount GET /api/flow/status overlaps route GET /api/flow/:id registered on the same method, and a mount owns its whole subtree",
			wantRows:  1,
		},
		{
			name: "a conflict found mid-way registers nothing",
			register: func(r *Router) {
				r.OPTIONS("/flow/status", okRouteHandler)
				r.Mount("/flow", mountedHandler())
			},
			wantPanic: "xbc: mount OPTIONS /api/flow overlaps route OPTIONS /api/flow/status registered on the same method, and a mount owns its whole subtree",
			wantRows:  1,
		},
		{
			name: "a sibling that only shares the prefix's characters",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.GET("/flowx", okRouteHandler)
			},
			wantRows: len(anyMethods) + 1,
		},
		{
			name: "a pattern route in a sibling subtree",
			register: func(r *Router) {
				r.GET("/flowx/*rest", okRouteHandler)
				r.Mount("/flow", mountedHandler())
			},
			wantRows: len(anyMethods) + 1,
		},
		{
			name: "a pattern route on a method the mount does not cover",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.Handle(http.MethodTrace, "/*all", okRouteHandler)
			},
			wantRows: len(anyMethods) + 1,
		},
		{
			name: "a route above a narrower mount",
			register: func(r *Router) {
				r.GET("/flow", okRouteHandler)
				r.Mount("/flow/status", mountedHandler())
			},
			wantRows: len(anyMethods) + 1,
		},
		{
			name: "a mount below a plain route's path",
			register: func(r *Router) {
				r.GET("/flow/status", okRouteHandler)
				r.Mount("/flow/status/live", mountedHandler())
			},
			wantRows: len(anyMethods) + 1,
		},
		{
			name: "a route on a method the mount does not cover",
			register: func(r *Router) {
				r.Mount("/flow", mountedHandler())
				r.Handle(http.MethodTrace, "/flow/status", okRouteHandler)
			},
			wantRows: len(anyMethods) + 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, _ := newTestRouter("/api")
			if tc.wantPanic != "" {
				require.PanicsWithValue(t, tc.wantPanic, func() { tc.register(router) })
			} else {
				require.NotPanics(t, func() { tc.register(router) })
			}
			assert.Len(t, *router.routes, tc.wantRows)
		})
	}
}

// TestMountRefusesAWildcardAtTheRoot pins the coarsest claim a pattern route
// makes: a catch-all in the path's first segment has no literal prefix at
// all, so it already reaches every mount on its method. Without it the pair
// would reach gin, which panics on the registration itself with a message
// naming neither side. The case needs its own router because the prefix a
// catch-all leaves behind is only empty at the root, and the conflict matrix
// registers under a group.
func TestMountRefusesAWildcardAtTheRoot(t *testing.T) {
	router, _ := newTestRouter("/")
	router.GET("/*all", okRouteHandler)

	require.PanicsWithValue(t,
		"xbc: mount GET /flow overlaps route GET /*all registered on the same method, and a mount owns its whole subtree",
		func() { router.Mount("/flow", mountedHandler()) })
	assert.Len(t, *router.routes, 1)
}

// TestPatternRoutesOutsideMountsKeepTheEngineConflict pins where the conflict
// rule stops. Two pattern routes sharing a prefix are one engine's routing
// syntax colliding inside that engine's own tree; xbc compares them like any
// other pair of routes and leaves the refusal, and its wording, to the
// engine. Only an overlap a mount is part of is xbc's to name.
func TestPatternRoutesOutsideMountsKeepTheEngineConflict(t *testing.T) {
	router, _ := newTestRouter("/api")
	require.NotPanics(t, func() {
		router.GET("/flow/:id", okRouteHandler)
		router.GET("/flow/*rest", okRouteHandler)
	})
	assert.Len(t, *router.routes, 2)
}

// TestFreezeRefusesAnUnmeteredMount pins the reason RouteInfo.Mounted has to
// exist: an exemption from the in-flight gate is keyed by one literal method
// and path, which a subtree does not have, so the combination is refused when
// the table freezes rather than left silently metered.
func TestFreezeRefusesAnUnmeteredMount(t *testing.T) {
	declared, _ := newTestRouter("/api")
	declared.Mount("/flow", mountedHandler())
	_, err := declared.freeze()
	require.NoError(t, err, "a mount without Unmetered must freeze cleanly")

	exempted, _ := newTestRouter("/api")
	exempted.Mount("/flow", mountedHandler()).Unmetered()
	_, err = exempted.freeze()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: mounted route GET /api/flow is marked unmetered")
}
