package web

import (
	"net"

	"github.com/xbcio/xbc/extensions/authentication"
)

// This file is the single seam through which package web's external tests
// (package web_test, driven by transport/web/enginetest) reach identifiers
// that stay unexported in production. It exists for the same reason net/http
// keeps its own test-only knobs behind an internal _test.go file: adding these
// to the public surface would leave every future reader of the production
// files wondering which of them a real caller is meant to use.
//
// Tests that need more than a name -- deeply internal state, or a gin-typed
// signature that cannot cross the package boundary at all -- stay in package
// web instead of widening this file; see router_internal_test.go for why an
// internal test file cannot reach enginetest.

// setListener injects a pre-bound net.Listener for Start to use instead of
// calling net.Listen itself.
func (s *Server) setListener(ln net.Listener) {
	s.listener = ln
}

// Route table construction and freezing. An external test builds the same
// three-value table (*Server).Start does, so the routes it registers pick up
// the recordCurrentRoute wiring Router.Handle bakes in.
var (
	NewRouteTable = newRouteTable
	NewRouter     = newRouter
	AnyMethods    = anyMethods
)

// Freeze exposes the route-table freeze that turns registrations into an
// immutable RouteCatalog.
func (r *Router) Freeze() (RouteCatalog, error) { return r.freeze() }

// AppendGlobalHandler appends a handler to this Router's own chain, the way a
// Middleware plugin's handler is installed during (*Server).Start. Tests use
// it to observe chain ordering -- above all where recordCurrentRoute sits
// relative to global middleware -- without reaching into r.handlers, whose
// aliasing rules appendChain owns.
func (r *Router) AppendGlobalHandler(handlers ...Handler) {
	r.handlers = appendChain(r.handlers, handlers...)
}

// RecordCurrentRoute is the closure Router.Handle bakes into the front of
// every route's flattened chain. Exposed so a test can assert on it directly
// rather than only through a dispatched request.
var RecordCurrentRoute = recordCurrentRoute

// Authentication. The middleware type stays unexported in production, but an
// external test needs to name it in a helper's signature, not merely infer it
// at a var declaration -- hence an alias rather than a bare constructor.
type AuthenticationMiddleware = authenticationMiddleware

// NewAuthenticationMiddleware is the constructor (*Server).Start calls.
var NewAuthenticationMiddleware = newAuthenticationMiddleware

// Credential collection. NewRequestCredentialSource returns the unexported
// per-request adapter; a test holds it through the authentication.CredentialSource
// interface it satisfies.
var (
	NewExtractorIndex = newExtractorIndex
	LimitRequestBody  = limitRequestBody
)

// NewRequestCredentialSource builds the per-request CredentialSource the
// authentication middleware hands to the manager.
func NewRequestCredentialSource(
	c *Ctx, extractors map[authentication.Scheme]CredentialExtractor,
) authentication.CredentialSource {
	return requestCredentialSource{ctx: c, extractors: extractors}
}

// PrincipalContextKey is the request-scoped key the framework publishes the
// authenticated Principal under.
const PrincipalContextKey = principalContextKey

// NewRouteCatalog builds a frozen catalog directly, without driving a whole
// Router. Tests use it to hand the authentication middleware a route table
// that deliberately disagrees with what a request reports as its current
// route, which a Router-produced catalog cannot express.
func NewRouteCatalog(routes []RouteInfo) RouteCatalog {
	index := make(map[string]RouteInfo, len(routes))
	for _, route := range routes {
		index[routeKey(route.Method, route.Path)] = route
	}
	return &routeCatalog{all: append([]RouteInfo(nil), routes...), index: index}
}

// SetCurrentRoute records route under the same private key recordCurrentRoute
// writes, standing in for it where a test needs one specific route recorded --
// above all one the compiled catalog does not contain.
func SetCurrentRoute(c *Ctx, route RouteInfo) { c.Set(currentRouteContextKey, route) }

// Error resolution. NewErrorResolver returns the unexported resolver type, so
// an external test can only hold it through inference -- which is all the
// tests need, since they drive it through Attach and then make requests.
var NewErrorResolver = newErrorResolver

// Attach publishes this resolver on the request so OnError and the error
// boundary downstream can find it.
func (r *errorResolver) Attach(c *Ctx) { r.attach(c) }

// ResolverFor looks up the resolver a request is carrying.
var ResolverFor = resolverFor

// ProblemContentType is the media type every Problem Detail response carries.
const ProblemContentType = problemContentType

// Server construction. NewServer is the constructor the plugin definition
// calls; a lifecycle test drives it directly so it can supply its own engine
// factory and plugin entries.
var NewServer = newServer

// NewErrorBoundary builds the error-boundary Middleware the plugin graph
// normally assembles from the collected ErrorMapper plugins. Every server a
// test starts needs one installed, or a handler's error reaches no mapper at
// all.
func NewErrorBoundary(mappers ...ErrorMapper) Middleware {
	return &errorBoundary{mappers: append([]ErrorMapper(nil), mappers...)}
}

// Engine returns the Engine a started Server built through its factory, so a
// test can dispatch requests into the assembled route tree with httptest
// instead of going over a real socket.
func (s *Server) Engine() Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}
