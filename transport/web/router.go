package web

import (
	"context"
	"fmt"
	"net/http"
	"path"

	"github.com/xbcio/xbc/extensions/authentication"
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
// method-specific registration helpers. It embeds *Router so every
// registration method (GET, POST, Group, Perm, Auth, ...) is promoted onto
// it, restoring gin's own cascaded call style: r.GET("/a", h).POST("/b", h)
// registers two routes on the same underlying group. A handle remains
// writable only until the shared route table is frozen; changing metadata
// afterwards panics for the same reason adding a late route does.
//
// Route.Perm and Route.Auth shadow the promoted Router.Perm and Router.Auth:
// Go's method resolution always picks the shallower declaration, so calling
// .Perm/.Auth on a *Route resolves to Route's own route-level method, never
// to Router's group-level default setter -- regardless of what the embedded
// *Router looks like. The same method name therefore means two different
// things depending on which type the variable holds:
//
//	g := router.Group("/x").Perm("A") // group-level default (receiver *Router)
//	g.GET("/y", h).Perm("B")          // route-level override (receiver *Route)
//
// This is a deliberate consequence of the embedding, not an accidental name
// collision -- see the design doc's improvement 8.
type Route struct {
	*Router // promotes every Router method, including the registration helpers
	// indexes are the route-table positions this handle covers. Single-method
	// registrations produce one; Any and Match produce one per method, so a
	// single .Perm call applies to every route the registration created.
	indexes []int
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
	// r.Router must be checked before r.routes/r.frozen: those two fields are
	// promoted from the embedded *Router, so reading them through a nil
	// *Router panics with an unhelpful nil-pointer-dereference runtime error
	// instead of this method's own diagnostic. Short-circuit evaluation of ||
	// guarantees r.Router is only read once r is known non-nil, and
	// r.routes/r.frozen are only read once r.Router is known non-nil.
	if r == nil || r.Router == nil || r.routes == nil || r.frozen == nil || len(r.indexes) == 0 {
		panic("xbc: invalid route metadata handle")
	}
	if *r.frozen {
		panic("xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata")
	}
	for _, index := range r.indexes {
		if index < 0 || index >= len(*r.routes) {
			panic("xbc: invalid route metadata handle")
		}
		fn(&(*r.routes)[index])
	}
}

// Router wraps a neutral Engine with route-table recording and a freeze
// switch. routes, frozen and index are pointers so every Router returned by
// Group shares the same underlying slice/flag/map as the root -- freezing
// the root freezes every group derived from it too.
//
// handlers is this group's own middleware chain, snapshotted when Group
// created it. The chain is accumulated here rather than handed to the engine
// because Engine (design §4.2) deliberately has no Group or Use: xbc
// flattens global + group + route handlers into one []Handler before
// registration, so the engine only ever sees one already-ordered chain per
// route.
//
// defaultPerm and defaultAuth are the opposite: plain values, copied by Group
// the same way basePath already is. They name a group-level policy default,
// and a default only makes sense scoped to the subtree that declared it --
// sharing it through a pointer would let a child's .Perm/.Auth call leak back
// into the parent and every sibling derived from the same root, which is
// exactly the cross-subtree bleed a per-group default exists to prevent.
type Router struct {
	engine      Engine
	handlers    []Handler
	basePath    string
	routes      *[]RouteInfo
	frozen      *bool
	index       *map[string]RouteInfo
	defaultPerm string
	defaultAuth *AuthPolicy
}

// newRouteTable allocates the three pieces of shared, pointer-identity
// state a Router and recordCurrentRoute's middleware both need to close
// over. It exists as a step separate from newRouter because
// recordCurrentRoute must be part of the initial handlers chain newRouter
// receives -- so (*Server).Start needs frozen/index available before a
// *Router object exists at all.
func newRouteTable() (routes *[]RouteInfo, frozen *bool, index *map[string]RouteInfo) {
	r := make([]RouteInfo, 0, 16)
	f := false
	i := make(map[string]RouteInfo)
	return &r, &f, &i
}

// newRouter is called exactly once, by (*Server).Start, the first time the
// route table needs somewhere to register routes. routes/frozen/index come
// from a prior newRouteTable call -- see that function's doc comment for why
// the two are split. handlers is the root chain every route inherits --
// (*Server).Start builds it in the exact order internal middleware must run
// (recordCurrentRoute, body-size limiting, the error boundary, then
// plugin-ordered middleware) before calling newRouter, so there is no
// ordering hazard left for this function to depend on: appendChain always
// copies, so the root Router owns its own chain from construction onward.
func newRouter(engine Engine, basePath string, handlers []Handler, routes *[]RouteInfo, frozen *bool, index *map[string]RouteInfo) *Router {
	return &Router{
		engine:   engine,
		handlers: appendChain(nil, handlers...),
		basePath: joinPaths("/", basePath),
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

// appendChain returns parent followed by extra in a freshly allocated slice.
// Allocating unconditionally is the entire point: append(parent, extra...)
// may write into parent's spare capacity, and two sibling groups derived from
// the same parent would then share -- and overwrite -- each other's
// middleware. See TestAppendChainNeverWritesIntoParentSpareCapacity.
func appendChain(parent []Handler, extra ...Handler) []Handler {
	chain := make([]Handler, 0, len(parent)+len(extra))
	chain = append(chain, parent...)
	return append(chain, extra...)
}

// Group returns a sub-router rooted at relativePath, sharing this router's
// route table and freeze flag. Handlers passed here run for every route
// registered on the returned sub-router and on any sub-router derived from it.
//
// This is the only way to scope middleware to part of the route tree: there is
// deliberately no Router.Use. The returned Router owns a snapshot of the chain
// taken at this call, so a later Use on the parent could not reach groups that
// already exist -- the middleware would appear registered while protecting
// nothing. Declaring the handlers at the point the group is created makes that
// failure mode unrepresentable.
func (r *Router) Group(relativePath string, h ...Handler) *Router {
	return &Router{
		engine:      r.engine,
		handlers:    appendChain(r.handlers, h...),
		basePath:    joinPaths(r.basePath, relativePath),
		routes:      r.routes,
		frozen:      r.frozen,
		index:       r.index,
		defaultPerm: r.defaultPerm,
		defaultAuth: cloneAuthPolicy(r.defaultAuth),
	}
}

// Perm sets this group's default permission: every route registered on this
// Router from this call onward, and on any sub-router derived afterward via
// Group, has it written into its RouteInfo at registration time unless that
// route's own .Perm call overrides it. The default is a snapshot, not a live
// reference -- see the Router.defaultPerm field comment -- so it follows the
// same ordering rule gin's own combineHandlers uses for parent handlers: a
// route or sub-group already registered before this call keeps what it had,
// and only registrations that come after see the new default.
//
// An empty permission means "not declared" everywhere else this package reads
// Perm, so passing "" here would silently make .Perm a no-op instead of
// clearing a previous default; call it with a non-empty permission.
func (r *Router) Perm(permission string) *Router {
	r.defaultPerm = permission
	return r
}

// Auth sets this group's default authentication policy, following the same
// registration-time snapshot semantics as Perm: it reaches routes and
// sub-groups registered after this call, never ones registered before it, and
// a route's own .Auth call always overrides the group default that would
// otherwise have applied. The policy is cloned on the way in, and cloned again
// per route when Handle writes it into a RouteInfo, so no two routes -- or
// this Router and a route derived from it -- ever share the same *AuthPolicy
// or its backing schemes slice.
func (r *Router) Auth(policy AuthPolicy) *Router {
	r.defaultAuth = cloneAuthPolicy(&policy)
	return r
}

// Handle registers a route and records it in the route table. The recorded
// RouteInfo starts from this Router's current defaultPerm/defaultAuth (see
// Router.Perm and Router.Auth), so group-level policy is written in at
// registration time rather than looked up from a parent later; the route
// table stays self-describing and RouteCatalog's enumeration never needs to
// know groups exist. A subsequent .Perm/.Auth call on the returned *Route
// still overrides whatever default was written here. Calling Handle after
// freeze panics -- RouteCatalogListener runs after the route table is
// supposed to be complete, so a plugin adding a route there is a
// programming mistake, not a runtime condition worth recovering from.
func (r *Router) Handle(method, relativePath string, h ...Handler) *Route {
	if *r.frozen {
		panic("xbc: route table is frozen, RouteCatalogListener phase cannot add routes")
	}
	fullPath := joinPaths(r.basePath, relativePath)
	r.engine.Handle(method, fullPath, appendChain(r.handlers, h...))
	*r.routes = append(*r.routes, RouteInfo{
		Method: method,
		Path:   fullPath,
		Auth:   cloneAuthPolicy(r.defaultAuth),
		Perm:   r.defaultPerm,
	})
	return &Route{Router: r, indexes: []int{len(*r.routes) - 1}}
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

func (r *Router) GET(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodGet, relativePath, h...)
}

func (r *Router) POST(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodPost, relativePath, h...)
}

func (r *Router) PUT(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodPut, relativePath, h...)
}

func (r *Router) DELETE(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodDelete, relativePath, h...)
}

func (r *Router) PATCH(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodPatch, relativePath, h...)
}

func (r *Router) HEAD(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodHead, relativePath, h...)
}

func (r *Router) OPTIONS(relativePath string, h ...Handler) *Route {
	return r.Handle(http.MethodOptions, relativePath, h...)
}

// anyMethods are the methods Any registers. It mirrors gin's own anyMethods
// (gin v1.12.0 routergroup.go) so Any behaves the same as the router it wraps,
// minus CONNECT and TRACE, which gin includes but which no XBC service should
// expose by default: CONNECT is for proxies and TRACE reflects request headers
// back to the caller, a known cross-site tracing vector.
var anyMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodHead, http.MethodOptions, http.MethodDelete,
}

// Any registers the same handlers for every method in anyMethods. The returned
// Route covers all of them, so one .Perm or .Auth call applies uniformly --
// a per-method policy needs per-method registration instead.
func (r *Router) Any(relativePath string, h ...Handler) *Route {
	return r.Match(anyMethods, relativePath, h...)
}

// Match registers the same handlers for each listed method. An empty method
// list is a programming mistake: it would register nothing while returning a
// handle whose .Perm silently applies to no route, which is exactly the kind
// of invisible policy gap the route table exists to prevent.
func (r *Router) Match(methods []string, relativePath string, h ...Handler) *Route {
	if len(methods) == 0 {
		panic("xbc: Match requires at least one HTTP method")
	}
	indexes := make([]int, 0, len(methods))
	for _, method := range methods {
		indexes = append(indexes, r.Handle(method, relativePath, h...).indexes...)
	}
	return &Route{Router: r, indexes: indexes}
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
// must be the first handler in the root chain (*Server).Start builds, ahead
// of any user-contributed middleware (design §5.6): the engine resolves the
// route -- and therefore the matched full path -- before invoking the
// handler chain at all, so even running first in that chain still sees the
// correct match; running it first is what guarantees every later
// middleware, including ones that fail or abort the chain early, can still
// call CurrentRoute.
//
// frozen/index are captured by pointer (from newRouteTable) so this handler
// can be installed before the route table is complete -- freeze() only
// fills in *index once every RegisterRoutes call has run -- and still see
// the final table by the time the first real request arrives, because
// Serve is only ever reached after Start (which calls freeze) has returned.
func recordCurrentRoute(frozen *bool, index *map[string]RouteInfo) Handler {
	return func(_ context.Context, c *Ctx) error {
		if frozen == nil || !*frozen {
			return nil
		}
		gc := c.Gin()
		if info, ok := (*index)[routeKey(gc.Request.Method, gc.FullPath())]; ok {
			c.Set(currentRouteContextKey, cloneRouteInfo(info))
		}
		return nil
	}
}

// CurrentRoute reports which frozen route table entry c is currently
// handling. It depends only on the internal middleware having already run
// on this request (see recordCurrentRoute) -- not on *plugin.Context or any
// mutable route table -- so a plugin that only wires up gRPC is never forced
// to import Gin just to have a Context capable of answering this question.
// It returns false for a request that matches no frozen route, or one made
// before the internal middleware has had a chance to run at all.
func CurrentRoute(c *Ctx) (RouteInfo, bool) {
	v, ok := c.Get(currentRouteContextKey)
	if !ok {
		return RouteInfo{}, false
	}
	info, ok := v.(RouteInfo)
	return cloneRouteInfo(info), ok
}
