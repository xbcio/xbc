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

func TestRouteCatalogAllReturnsDefensiveCopy(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/")
	router.GET("/a", func(*gin.Context) {})
	router.GET("/b", func(*gin.Context) {})
	catalog := router.freeze()

	got := catalog.All()
	require.Len(t, got, 2)
	got[0].Path = "/mutated"

	again := catalog.All()
	assert.NotEqual(t, "/mutated", again[0].Path, "调用方拿到的切片被改动，不能影响 catalog 自身或后续的 All() 调用")
}

func TestRouteCatalogLookupHitAndMiss(t *testing.T) {
	_, router, _, _ := newTestEngineAndRouter("/api")
	router.GET("/users", func(*gin.Context) {})
	catalog := router.freeze()

	info, ok := catalog.Lookup(http.MethodGet, "/api/users")
	require.True(t, ok, "已注册的方法+路径组合必须命中")
	assert.Equal(t, RouteInfo{Method: http.MethodGet, Path: "/api/users"}, info)

	_, ok = catalog.Lookup(http.MethodPost, "/api/users")
	assert.False(t, ok, "方法不同，即便路径相同，也不能命中")

	_, ok = catalog.Lookup(http.MethodGet, "/api/orders")
	assert.False(t, ok, "从未注册过的路径必须未命中")
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
	router.freeze()

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.True(t, ok, "命中的请求必须能拿到 CurrentRoute")
	assert.Equal(t, RouteInfo{Method: http.MethodGet, Path: "/api/users/:id"}, got,
		"CurrentRoute 必须报告冻结路由表里带路径参数占位符的原始 Path，而不是请求里的实际值")
}

func TestCurrentRouteReportsFalseForNonMatchingRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	routes, frozen, index := newRouteTable()
	engine.Use(recordCurrentRoute(frozen, index))
	router := newRouter(engine, "/api", routes, frozen, index)
	router.GET("/users", func(*gin.Context) {})
	router.freeze()

	var ok bool
	engine.NoRoute(func(gc *gin.Context) {
		_, ok = CurrentRoute(gc)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ok, "未命中任何冻结路由的请求，CurrentRoute 必须返回 false")
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
	router.freeze()

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.True(t, ran, "engine.Use() 在 newRouter 建组之前调用，中间件必须真正作用于 basePath 下的路由")
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
	router.freeze()

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	assert.False(t, ran, "Group 先于 Use 建立时，之后追加的中间件不会作用于已经存在的分组——这正是 newRouter 文档警告的坑")
	assert.Equal(t, http.StatusOK, rec.Code, "路由本身仍应正常命中，只是中间件没有跑")
}
