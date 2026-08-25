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
// switch. routes and frozen are pointers so every Router returned by Group
// shares the same underlying slice/flag as the root -- freezing the root
// freezes every group derived from it too.
type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
}

// newRouter is called exactly once, at stage 7, the first time AssembleHTTP
// needs somewhere to register routes.
func newRouter(engine *gin.Engine, basePath string) *Router {
	routes := make([]RouteInfo, 0, 16)
	frozen := false
	return &Router{
		engine:   engine,
		group:    engine.Group(basePath),
		basePath: basePath,
		routes:   &routes,
		frozen:   &frozen,
	}
}

// freeze locks the route table. Called once, right after every plugin's
// RegisterRoutes has run and before any PostRoutes runs.
func (r *Router) freeze() { *r.frozen = true }

// Group returns a sub-router rooted at relativePath, sharing this router's
// route table and freeze flag.
func (r *Router) Group(relativePath string) *Router {
	return &Router{
		engine:   r.engine,
		group:    r.group.Group(relativePath),
		basePath: r.basePath,
		routes:   r.routes,
		frozen:   r.frozen,
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
		Path:   path.Join(r.group.BasePath(), relativePath),
	})
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
