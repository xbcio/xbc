package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngineAndRouter(basePath string) (*gin.Engine, *Router, *bool, *map[string]RouteInfo) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes, frozen, index := newRouteTable()
	router := newRouter(engine, basePath, routes, frozen, index)
	return engine, router, frozen, index
}

func TestRouteMetadataChainIsFrozenIntoCatalogAndCurrentRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes, frozen, index := newRouteTable()
	engine.Use(recordCurrentRoute(frozen, index))
	router := newRouter(engine, "/api", routes, frozen, index)

	var current RouteInfo
	router.Group("/users").POST("", func(gc *gin.Context) {
		current, _ = CurrentRoute(gc)
	}).Name("create user").Perm("user:write").Idempotent()
	router.POST("/login", func(*gin.Context) {}).Name("login").Auth(Public())
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

func TestRouteMetadataCannotChangeAfterFreeze(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/")
	route := router.GET("/users", func(*gin.Context) {})
	_, err := router.freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata",
		func() { route.Auth(Public()) },
	)
}

func TestRouteCatalogAllReturnsDefensiveCopy(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/")
	router.GET("/a", func(*gin.Context) {})
	router.GET("/b", func(*gin.Context) {})
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
	router.GET("/users", func(*gin.Context) {})
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
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes, frozen, index := newRouteTable()
	// recordCurrentRoute must be installed via engine.Use() before newRouter
	// creates the base-path group -- see newRouter's own doc comment.
	engine.Use(recordCurrentRoute(frozen, index))
	router := newRouter(engine, "/api", routes, frozen, index)

	var got RouteInfo
	var ok bool
	router.GET("/users/:id", func(gc *gin.Context) {
		got, ok = CurrentRoute(gc)
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
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes, frozen, index := newRouteTable()
	engine.Use(recordCurrentRoute(frozen, index))
	router := newRouter(engine, "/api", routes, frozen, index)
	router.GET("/users", func(*gin.Context) {})
	_, err := router.freeze()
	require.NoError(t, err)

	var ok bool
	engine.NoRoute(func(gc *gin.Context) {
		_, ok = CurrentRoute(gc)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ok, "No request matched any frozen route, CurrentRoute must return false")
}

// TestGroupMustBeCreatedAfterUseOrMiddlewareSilentlyNeverApplies pins the
// hard ordering constraint documented on newRouter: gin's
// RouterGroup.Group snapshots the parent's Handlers slice by value at call
// time, so any engine.Use() call made *after* the base-path group already
// exists never reaches requests through that group. This is exactly the
// silent-regression risk design §5.6/§8.1 calls out, so it needs a fixture
// that actually dispatches an HTTP request through the group, not just an
// inspection of some internal slice.
func TestGroupMustBeCreatedAfterUseOrMiddlewareSilentlyNeverApplies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var ran bool
	mw := func(gc *gin.Context) { ran = true }

	// Correct order: every engine.Use() call happens before newRouter grabs
	// engine.Group(basePath) -- mirrors (*Server).Start's own sequencing.
	engine.Use(mw)
	routes, frozen, index := newRouteTable()
	router := newRouter(engine, "/api", routes, frozen, index)
	router.GET("/ping", func(gc *gin.Context) { gc.Status(http.StatusOK) })
	_, err := router.freeze()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.True(t, ran, "engine.Use() was called before newRouter group creation, middleware must actually apply to routes under basePath")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestGroupCreatedBeforeUseNeverSeesLaterMiddleware is the mutation-proving
// counterpart of the test above: it deliberately reproduces the *wrong*
// order (Group before Use) to demonstrate the failure mode newRouter's doc
// comment warns about, pinning that gin really does behave this way rather
// than trusting the doc comment's claim on faith.
func TestGroupCreatedBeforeUseNeverSeesLaterMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var ran bool
	mw := func(gc *gin.Context) { ran = true }

	// Wrong order: the group is created first, and Use() is only called on
	// the engine afterward.
	routes, frozen, index := newRouteTable()
	router := newRouter(engine, "/api", routes, frozen, index)
	engine.Use(mw)
	router.GET("/ping", func(gc *gin.Context) { gc.Status(http.StatusOK) })
	_, err := router.freeze()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ran, "When group is created before Use, subsequent middleware won't apply to existing groups - this is exactly the pitfall warned about in newRouter documentation")
	assert.Equal(t, http.StatusOK, rec.Code, "The route itself should still match normally, but the middleware won't run")
}
