package web

import (
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

// RouteInfo is one entry in the frozen route table.
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

// newRouteTable allocates the three pieces of shared, pointer-identity
// state a Router and recordCurrentRoute's middleware both need to close
// over. It exists as a step separate from newRouter because
// recordCurrentRoute must be installed via engine.Use() *before* newRouter
// runs engine.Group (see newRouter's doc comment) -- so (*Server).Start
// needs frozen/index available before a *Router object exists at all.
func newRouteTable() (routes *[]RouteInfo, frozen *bool, index *map[string]RouteInfo) {
	r := make([]RouteInfo, 0, 16)
	f := false
	i := make(map[string]RouteInfo)
	return &r, &f, &i
}

// newRouter is called exactly once, by (*Server).Start, the first time the
// route table needs somewhere to register routes. routes/frozen/index come
// from a prior newRouteTable call -- see that function's doc comment for
// why the two are split.
//
// engine.Group(basePath) here must run after every engine.Use() call the
// caller has already made, not before: gin's RouterGroup.Group snapshots the
// parent group's Handlers slice by value at call time (routergroup.go's
// combineHandlers copies, it never re-reads the parent later), so a group
// created before Use() would keep routing through an empty middleware chain
// forever, no matter how many handlers Use() adds to the engine afterward --
// silently serving requests with none of the ordered middleware ever
// running. (*Server).Start is written to respect this ordering; newRouter
// itself cannot enforce it because it only ever sees the engine at the
// moment it is called.
func newRouter(engine *gin.Engine, basePath string, routes *[]RouteInfo, frozen *bool, index *map[string]RouteInfo) *Router {
	return &Router{
		engine:   engine,
		group:    engine.Group(basePath),
		basePath: basePath,
		routes:   routes,
		frozen:   frozen,
		index:    index,
	}
}

// routeKey is the frozen index's key shape: method and path joined so that
// two routes differing only in method never collide.
func routeKey(method, path string) string {
	return method + " " + path
}

// freeze locks the route table -- called once, right after every plugin's
// RegisterRoutes has run and before any RouteCatalogConsumer runs -- and
// compiles it into a method+path-keyed map so CurrentRoute's request-time
// lookup (via the internal middleware in server.go) is a map hit rather than
// a scan. It returns an immutable RouteCatalog wrapping a defensive copy of
// the table, which is the only handle RouteCatalogConsumer.RoutesReady ever
// sees -- callers cannot reach back into the mutable *routes/*index this
// Router still holds.
func (r *Router) freeze() RouteCatalog {
	*r.frozen = true
	idx := make(map[string]RouteInfo, len(*r.routes))
	for _, ri := range *r.routes {
		idx[routeKey(ri.Method, ri.Path)] = ri
	}
	*r.index = idx

	all := make([]RouteInfo, len(*r.routes))
	copy(all, *r.routes)
	frozenIdx := make(map[string]RouteInfo, len(idx))
	for k, v := range idx {
		frozenIdx[k] = v
	}
	return &routeCatalog{all: all, index: frozenIdx}
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
// after freeze panics -- RouteCatalogConsumer runs after the route table is
// supposed to be complete, so a plugin adding a route there is a
// programming mistake, not a runtime condition worth recovering from.
func (r *Router) Handle(method, relativePath string, h ...gin.HandlerFunc) {
	if *r.frozen {
		panic("xbc: route table is frozen, RouteCatalogConsumer phase cannot add routes")
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
// (*gin.Context).FullPath() at request time -- always keeps. CurrentRoute's
// entire contract depends on RouteInfo.Path matching FullPath() by exact
// string equality, so a route registered with a trailing slash must record
// that trailing slash here too, or CurrentRoute silently returns false for
// every request to it.
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

// RouteCatalog is the frozen, read-only route table handed to
// RouteCatalogConsumer.RoutesReady. All returns a defensive copy; Lookup
// returns a value, never a pointer into any internal storage -- neither
// method gives a caller any way to mutate the table this Server actually
// dispatches against.
type RouteCatalog interface {
	All() []RouteInfo
	Lookup(method, path string) (RouteInfo, bool)
}

// routeCatalog is RouteCatalog's only implementation, produced by
// (*Router).freeze. Both fields are copies taken at freeze time, independent
// of the Router's own *routes/*index storage.
type routeCatalog struct {
	all   []RouteInfo
	index map[string]RouteInfo
}

// All returns a defensive copy: mutating the returned slice must never
// affect this catalog or any other call to All.
func (c *routeCatalog) All() []RouteInfo {
	out := make([]RouteInfo, len(c.all))
	copy(out, c.all)
	return out
}

// Lookup returns a value copy of the matching RouteInfo, never a pointer
// into c.index, so a caller cannot reach back in and mutate the catalog's
// own storage through the returned value.
func (c *routeCatalog) Lookup(method, path string) (RouteInfo, bool) {
	info, ok := c.index[routeKey(method, path)]
	return info, ok
}

// currentRouteContextKey is the gin.Context key the internal, outermost
// middleware installed by (*Server).Start writes the matched RouteInfo
// under. It is unexported and namespaced defensively (nothing else should
// ever legitimately write this key), because CurrentRoute's whole contract
// depends on nobody else colliding with it.
const currentRouteContextKey = "xbc/web.currentRoute"

// recordCurrentRoute is the internal middleware CurrentRoute depends on. It
// must be installed via engine.Use() before any user-contributed middleware
// (design §5.6): gin resolves the route -- and therefore gc.FullPath() --
// before invoking the handler chain at all, so even running first in that
// chain still sees the correct FullPath; running it first is what
// guarantees every later middleware, including ones that fail or abort the
// chain early, can still call CurrentRoute.
//
// frozen/index are captured by pointer (from newRouteTable) so this handler
// can be installed before the route table is complete -- freeze() only
// fills in *index once every RegisterRoutes call has run -- and still see
// the final table by the time the first real request arrives, because
// Serve is only ever reached after Start (which calls freeze) has returned.
func recordCurrentRoute(frozen *bool, index *map[string]RouteInfo) gin.HandlerFunc {
	return func(gc *gin.Context) {
		if frozen == nil || !*frozen {
			return
		}
		if info, ok := (*index)[routeKey(gc.Request.Method, gc.FullPath())]; ok {
			gc.Set(currentRouteContextKey, info)
		}
	}
}

// CurrentRoute reports which frozen route table entry gc is currently
// handling. It depends only on the internal middleware having already run
// on this request (see recordCurrentRoute) -- not on *plugin.Context or any
// mutable route table -- so a plugin that only wires up gRPC is never forced
// to import Gin just to have a Context capable of answering this question.
// It returns false for a request that matches no frozen route, or one made
// before the internal middleware has had a chance to run at all.
func CurrentRoute(gc *gin.Context) (RouteInfo, bool) {
	v, ok := gc.Get(currentRouteContextKey)
	if !ok {
		return RouteInfo{}, false
	}
	info, ok := v.(RouteInfo)
	return info, ok
}
