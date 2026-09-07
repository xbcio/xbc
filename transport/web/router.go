package web

import (
	"fmt"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
	"github.com/xbcio/xbc/authentication"
)

// AuthPolicy is a sealed, route-level authentication override. Its fields are
// deliberately private: callers can only construct the public and
// explicit-scheme variants through Public and Accepts, so those mutually
// exclusive states cannot be combined.
//
// A nil *AuthPolicy on RouteInfo is a third, distinct state. It means the route
// did not declare an override and must use the authentication manager's
// restrictive default; it never means public.
type AuthPolicy struct {
	kind    authPolicyKind
	schemes []authentication.Scheme
}

type authPolicyKind uint8

const (
	authPolicyInvalid authPolicyKind = iota
	authPolicyPublic
	authPolicyExplicit
)

// Public constructs a policy that bypasses authentication for a route.
func Public() AuthPolicy {
	return AuthPolicy{kind: authPolicyPublic}
}

// Accepts constructs a policy that restricts a route to the supplied
// authentication schemes. The input is copied immediately. Empty and duplicate
// schemes are rejected when the route table is frozen, where an error can name
// the affected route.
func Accepts(schemes ...authentication.Scheme) AuthPolicy {
	return AuthPolicy{
		kind:    authPolicyExplicit,
		schemes: append([]authentication.Scheme(nil), schemes...),
	}
}

// IsPublic reports whether this is the explicit public policy. In particular,
// a nil policy (the RouteInfo zero value) is not public.
func (p *AuthPolicy) IsPublic() bool {
	return p != nil && p.kind == authPolicyPublic
}

// Schemes returns the explicit policy's accepted schemes. It always returns a
// defensive copy, and returns nil for public, absent, or invalid policies.
func (p *AuthPolicy) Schemes() []authentication.Scheme {
	if p == nil || p.kind != authPolicyExplicit {
		return nil
	}
	return append([]authentication.Scheme(nil), p.schemes...)
}

func cloneAuthPolicy(policy *AuthPolicy) *AuthPolicy {
	if policy == nil {
		return nil
	}
	return &AuthPolicy{
		kind:    policy.kind,
		schemes: append([]authentication.Scheme(nil), policy.schemes...),
	}
}

func cloneRouteInfo(info RouteInfo) RouteInfo {
	info.Auth = cloneAuthPolicy(info.Auth)
	return info
}

func cloneRouteInfos(routes []RouteInfo) []RouteInfo {
	out := make([]RouteInfo, len(routes))
	for i, route := range routes {
		out[i] = cloneRouteInfo(route)
	}
	return out
}

// RouteInfo is one entry in the frozen route table. Metadata is declared next
// to route registration through Route's chainable methods and becomes
// immutable when the table is frozen.
type RouteInfo struct {
	Method     string
	Path       string
	Name       string
	Auth       *AuthPolicy
	Perm       string
	Idempotent bool
}

// Route is the metadata handle returned by Router.Handle and the
// method-specific registration helpers. A handle remains writable only until
// the shared route table is frozen; changing metadata afterwards panics for
// the same reason adding a late route does.
type Route struct {
	routes *[]RouteInfo
	frozen *bool
	index  int
}

// Name sets the human-readable operation name used by documentation and
// operational tooling.
func (r *Route) Name(name string) *Route {
	r.update(func(info *RouteInfo) { info.Name = name })
	return r
}

// Auth sets the route's authentication policy. Omitting Auth leaves the policy
// absent, which selects the authentication manager's restrictive default.
func (r *Route) Auth(policy AuthPolicy) *Route {
	r.update(func(info *RouteInfo) { info.Auth = cloneAuthPolicy(&policy) })
	return r
}

// Perm associates an authorization permission with the route.
func (r *Route) Perm(permission string) *Route {
	r.update(func(info *RouteInfo) { info.Perm = permission })
	return r
}

// Idempotent marks the operation as safe to retry with the same request
// semantics. Enforcement, if desired, is supplied by a separate plugin.
func (r *Route) Idempotent() *Route {
	r.update(func(info *RouteInfo) { info.Idempotent = true })
	return r
}

func (r *Route) update(fn func(*RouteInfo)) {
	if r == nil || r.routes == nil || r.frozen == nil || r.index < 0 || r.index >= len(*r.routes) {
		panic("xbc: invalid route metadata handle")
	}
	if *r.frozen {
		panic("xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata")
	}
	fn(&(*r.routes)[r.index])
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
// RegisterRoutes has run and before any RouteCatalogListener runs -- and
// compiles it into a method+path-keyed map so CurrentRoute's request-time
// lookup (via the internal middleware in server.go) is a map hit rather than
// a scan. It returns an immutable RouteCatalog wrapping a defensive copy of
// the table, which is the only handle RouteCatalogListener.RoutesReady ever
// sees -- callers cannot reach back into the mutable *routes/*index this
// Router still holds. Authentication policies are validated before any frozen
// state is published, so a failed freeze leaves the table mutable and returns
// no partial catalog.
func (r *Router) freeze() (RouteCatalog, error) {
	for _, route := range *r.routes {
		if err := validateRouteAuth(route); err != nil {
			return nil, err
		}
	}

	idx := make(map[string]RouteInfo, len(*r.routes))
	for _, ri := range *r.routes {
		idx[routeKey(ri.Method, ri.Path)] = cloneRouteInfo(ri)
	}
	*r.frozen = true
	*r.index = idx

	all := cloneRouteInfos(*r.routes)
	frozenIdx := make(map[string]RouteInfo, len(idx))
	for k, v := range idx {
		frozenIdx[k] = cloneRouteInfo(v)
	}
	return &routeCatalog{all: all, index: frozenIdx}, nil
}

func validateRouteAuth(route RouteInfo) error {
	policy := route.Auth
	if policy == nil {
		return nil
	}

	switch policy.kind {
	case authPolicyPublic:
		return nil
	case authPolicyExplicit:
		if len(policy.schemes) == 0 {
			return fmt.Errorf("xbc: route %s %s accepts no authentication schemes", route.Method, route.Path)
		}
		seen := make(map[authentication.Scheme]struct{}, len(policy.schemes))
		for i, scheme := range policy.schemes {
			if scheme == "" {
				return fmt.Errorf("xbc: route %s %s has an empty authentication scheme at index %d", route.Method, route.Path, i)
			}
			if _, duplicate := seen[scheme]; duplicate {
				return fmt.Errorf("xbc: route %s %s accepts duplicate authentication scheme %q", route.Method, route.Path, scheme)
			}
			seen[scheme] = struct{}{}
		}
		return nil
	default:
		return fmt.Errorf("xbc: route %s %s has an invalid authentication policy", route.Method, route.Path)
	}
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
// after freeze panics -- RouteCatalogListener runs after the route table is
// supposed to be complete, so a plugin adding a route there is a
// programming mistake, not a runtime condition worth recovering from.
func (r *Router) Handle(method, relativePath string, h ...gin.HandlerFunc) *Route {
	if *r.frozen {
		panic("xbc: route table is frozen, RouteCatalogListener phase cannot add routes")
	}
	r.group.Handle(method, relativePath, h...)
	*r.routes = append(*r.routes, RouteInfo{
		Method: method,
		Path:   joinPaths(r.group.BasePath(), relativePath),
	})
	return &Route{routes: r.routes, frozen: r.frozen, index: len(*r.routes) - 1}
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

func (r *Router) GET(relativePath string, h ...gin.HandlerFunc) *Route {
	return r.Handle(http.MethodGet, relativePath, h...)
}

func (r *Router) POST(relativePath string, h ...gin.HandlerFunc) *Route {
	return r.Handle(http.MethodPost, relativePath, h...)
}

func (r *Router) PUT(relativePath string, h ...gin.HandlerFunc) *Route {
	return r.Handle(http.MethodPut, relativePath, h...)
}

func (r *Router) DELETE(relativePath string, h ...gin.HandlerFunc) *Route {
	return r.Handle(http.MethodDelete, relativePath, h...)
}

func (r *Router) PATCH(relativePath string, h ...gin.HandlerFunc) *Route {
	return r.Handle(http.MethodPatch, relativePath, h...)
}

// RouteCatalog is the frozen, read-only route table handed to
// RouteCatalogListener.RoutesReady. All returns a defensive copy; Lookup
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
	return cloneRouteInfos(c.all)
}

// Lookup returns a value copy of the matching RouteInfo, never a pointer
// into c.index, so a caller cannot reach back in and mutate the catalog's
// own storage through the returned value.
func (c *routeCatalog) Lookup(method, path string) (RouteInfo, bool) {
	info, ok := c.index[routeKey(method, path)]
	return cloneRouteInfo(info), ok
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
			gc.Set(currentRouteContextKey, cloneRouteInfo(info))
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
	return cloneRouteInfo(info), ok
}
