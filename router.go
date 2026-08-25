package xbc

import (
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

// RouteInfo is one entry in the frozen route table. Plan 2 freezes method and
// path only; Plan 3 adds the metadata fields (Public, Name, Doc).
type RouteInfo struct {
	Method string
	Path   string
}

// Router wraps a *gin.RouterGroup with route-table recording and a freeze
// switch. routes, frozen and index are pointers so every Router returned by
// Group shares the same underlying slice/flag/map as the root -- freezing
// the root freezes every group derived from it too.
type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
	index    *map[string]RouteInfo
}

// newRouter is called exactly once, at stage 7, the first time AssembleHTTP
// needs somewhere to register routes.
func newRouter(engine *gin.Engine, basePath string) *Router {
	routes := make([]RouteInfo, 0, 16)
	frozen := false
	index := make(map[string]RouteInfo)
	return &Router{
		engine:   engine,
		group:    engine.Group(basePath),
		basePath: basePath,
		routes:   &routes,
		frozen:   &frozen,
		index:    &index,
	}
}

// freeze locks the route table. Called once, right after every plugin's
// RegisterRoutes has run and before any PostRoutes runs.
//
// It also compiles routes into a method+path-keyed map. This is not a
// performance optimization -- no scan-vs-map benchmark has been run, and at
// realistic route-table sizes a linear scan would likely be fine too, so
// nothing here is claiming the scan was slow. It exists solely because
// spec §827 documents the frozen route table as exactly this structure
// ("编成 map[method+path]*RouteInfo ... 冻结"), and Context.Route's request-time
// lookup must match what the spec describes, not a second, independently
// shaped structure that merely produces the same answers today.
func (r *Router) freeze() {
	*r.frozen = true
	idx := make(map[string]RouteInfo, len(*r.routes))
	for _, ri := range *r.routes {
		idx[routeKey(ri.Method, ri.Path)] = ri
	}
	*r.index = idx
}

// routeKey is the frozen index's key shape: method and path joined so that
// two routes differing only in method never collide.
func routeKey(method, path string) string {
	return method + " " + path
}

// Group returns a sub-router rooted at relativePath, sharing this router's
// route table and freeze flag.
func (r *Router) Group(relativePath string) *Router {
	return &Router{
		engine:   r.engine,
		group:    r.group.Group(relativePath),
		basePath: r.basePath,
		routes:   r.routes,
		frozen:   r.frozen,
		index:    r.index,
	}
}

// Handle registers a route and records it in the route table. Calling it
// after freeze panics -- PostRoutes runs after the route table is supposed
// to be complete, so a plugin adding a route there is a programming
// mistake, not a runtime condition worth recovering from.
func (r *Router) Handle(method, relativePath string, h ...gin.HandlerFunc) {
	if *r.frozen {
		panic("xbc: 路由表已在阶段 7 冻结，PostRoutes 里不能再加路由")
	}
	r.group.Handle(method, relativePath, h...)
	*r.routes = append(*r.routes, RouteInfo{
		Method: method,
		Path:   joinPaths(r.group.BasePath(), relativePath),
	})
}

// joinPaths mirrors gin's own private routergroup.go joinPaths (gin
// v1.12.0), instead of the bare path.Join this used to call. path.Join alone
// silently drops a trailing slash that gin's own router -- and therefore
// (*gin.Context).FullPath() at request time -- always keeps. Context.Route's
// entire contract depends on RouteInfo.Path matching FullPath() by exact
// string equality (spec §827: "c.FullPath() 返回的正是注册时的路由模板，与
// RouteInfo.Path 天然对齐"), so a route registered with a trailing slash must
// record that trailing slash here too, or Context.Route silently returns nil
// for every request to it.
func joinPaths(absolutePath, relativePath string) string {
	if relativePath == "" {
		return absolutePath
	}
	finalPath := path.Join(absolutePath, relativePath)
	if relativePath[len(relativePath)-1] == '/' && finalPath[len(finalPath)-1] != '/' {
		return finalPath + "/"
	}
	return finalPath
}

func (r *Router) GET(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodGet, relativePath, h...)
}

func (r *Router) POST(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPost, relativePath, h...)
}

func (r *Router) PUT(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPut, relativePath, h...)
}

func (r *Router) DELETE(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodDelete, relativePath, h...)
}

func (r *Router) PATCH(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPatch, relativePath, h...)
}
