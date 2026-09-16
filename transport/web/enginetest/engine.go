// Package enginetest provides a web.Engine implemented with nothing but the
// standard library.
//
// It exists so transport/web and its extension packages can drive a complete
// request path -- registration, chain execution, response writing, graceful
// shutdown -- without depending on any concrete engine. That is precisely what
// lets their module manifests drop the engine dependency: a test that needs a
// live server, a matched route, or a real writer no longer has to import one.
//
// It is a test engine and nothing else, and it must never be selected by an
// application. Three differences from a production engine are deliberate and
// permanent:
//
//   - Route patterns are http.ServeMux patterns ("GET /items/{id}"), not any
//     engine's own wildcard syntax. Routing is delegated to ServeMux; this
//     package implements no matching of its own.
//   - ClientIP reports the host part of RemoteAddr. There is no trusted-proxy
//     concept here, so forwarding headers are never consulted. A test that
//     needs to pin trusted-proxy behaviour must run against the engine that
//     implements it.
//   - Bind and BindURI are not implemented. Binding is an engine's own
//     pipeline, and a second one written here would be a different pipeline
//     wearing the same name. Both report a refusal, which Ctx adapts through
//     ParamError like any other binding failure.
package enginetest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

// unmatchedPattern is the catch-all ServeMux registers so that an unmatched
// request reaches this package's own NoRoute/NoMethod chains instead of
// ServeMux's built-in 404 and 405 responses, which would bypass them.
const unmatchedPattern = "/"

// Engine is the standard-library web.Engine. Its port methods (Handle,
// NoRoute, NoMethod, Serve, Shutdown) implement web.Engine verbatim; the
// separately named GET/POST/... helpers below exist only for tests that want
// to register a route without standing up a Router.
type Engine struct {
	mux *http.ServeMux
	srv *http.Server

	// chain is the accumulated global chain the convenience helpers prepend.
	// The port's Handle never reads it: flattening belongs to the caller, the
	// same division of labour web.Router and web.Engine already have.
	chain []web.Handler

	noRoute  []web.Handler
	noMethod []web.Handler
}

// New builds an Engine with no routes and no global chain.
func New() *Engine {
	e := &Engine{mux: http.NewServeMux()}
	e.srv = &http.Server{Handler: e}
	e.mux.Handle(unmatchedPattern, http.HandlerFunc(e.serveUnmatched))
	return e
}

// Factory adapts New to web.EngineFactory so a Server under test can be given
// this engine the same way an application is given a real one.
type Factory struct{}

// NewEngine honours the Options that map onto an http.Server. TrustedProxies
// and MaxMultipartMemory have no counterpart here and are ignored; see the
// package documentation for why that is deliberate rather than pending.
func (Factory) NewEngine(opts web.Options) (web.Engine, error) {
	e := New()
	e.srv.ReadTimeout = opts.ReadTimeout
	e.srv.ReadHeaderTimeout = opts.ReadHeaderTimeout
	e.srv.WriteTimeout = opts.WriteTimeout
	e.srv.IdleTimeout = opts.IdleTimeout
	e.srv.MaxHeaderBytes = opts.MaxHeaderBytes
	return e, nil
}

// -- web.Engine --

// Handle registers chain under method and path exactly as given. It splices
// nothing: the caller hands over one already-flattened chain per route.
func (e *Engine) Handle(method, path string, chain []web.Handler) {
	registered := slices.Clone(chain)
	e.mux.Handle(method+" "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run(registered, w, r)
	}))
}

// NoRoute installs the chain that answers a request no registered pattern
// matches.
func (e *Engine) NoRoute(chain []web.Handler) { e.noRoute = slices.Clone(chain) }

// NoMethod installs the chain that answers a request whose path is registered
// only under other methods.
//
// Honouring this needed care. ServeMux answers such a request with its own 405
// before any handler runs, which would silently bypass this chain; registering
// a catch-all pattern suppresses that, but then a 405 request and a 404 request
// arrive at the same place. The two are told apart by re-asking ServeMux the
// same request under each other method, so the answer still comes from
// ServeMux's matcher rather than from a second matcher written here. That same
// re-ask yields the Allow header the port requires on a 405.
func (e *Engine) NoMethod(chain []web.Handler) { e.noMethod = slices.Clone(chain) }

// Serve serves on ln until Shutdown.
func (e *Engine) Serve(ln net.Listener) error { return e.srv.Serve(ln) }

// Shutdown drains in-flight requests until ctx is done, then force-closes
// whatever refused to drain. The forced close is what makes the web.Engine
// contract hold: returning the drain error on its own would leave established
// connections serving past the deadline the caller asked to stop within. The
// drain error is still reported, so a caller can tell a clean drain from a
// deadline it had to be rescued from.
func (e *Engine) Shutdown(ctx context.Context) error {
	err := e.srv.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if closeErr := e.srv.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("xbc: graceful shutdown failed (%v) and forced close failed: %w", err, closeErr)
	}
	return err
}

// ServeHTTP lets the Engine be driven directly by httptest, without a
// listener.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.mux.ServeHTTP(w, r) }

// -- test convenience --

// Use appends handlers to the global chain every later GET/POST/... helper
// prepends to its route chain. It has no effect on routes already registered,
// which matches how a Router snapshots its handlers at registration time.
func (e *Engine) Use(handlers ...web.Handler) { e.chain = append(e.chain, handlers...) }

// GET registers handlers for a GET route, behind the accumulated global chain.
func (e *Engine) GET(path string, handlers ...web.Handler) {
	e.register(http.MethodGet, path, handlers)
}

// POST registers handlers for a POST route, behind the accumulated global chain.
func (e *Engine) POST(path string, handlers ...web.Handler) {
	e.register(http.MethodPost, path, handlers)
}

// PUT registers handlers for a PUT route, behind the accumulated global chain.
func (e *Engine) PUT(path string, handlers ...web.Handler) {
	e.register(http.MethodPut, path, handlers)
}

// DELETE registers handlers for a DELETE route, behind the accumulated global chain.
func (e *Engine) DELETE(path string, handlers ...web.Handler) {
	e.register(http.MethodDelete, path, handlers)
}

// PATCH registers handlers for a PATCH route, behind the accumulated global chain.
func (e *Engine) PATCH(path string, handlers ...web.Handler) {
	e.register(http.MethodPatch, path, handlers)
}

// HEAD registers handlers for a HEAD route, behind the accumulated global chain.
func (e *Engine) HEAD(path string, handlers ...web.Handler) {
	e.register(http.MethodHead, path, handlers)
}

// OPTIONS registers handlers for an OPTIONS route, behind the accumulated global chain.
func (e *Engine) OPTIONS(path string, handlers ...web.Handler) {
	e.register(http.MethodOptions, path, handlers)
}

func (e *Engine) register(method, path string, handlers []web.Handler) {
	e.Handle(method, path, append(slices.Clone(e.chain), handlers...))
}

// -- unmatched requests --

func (e *Engine) serveUnmatched(w http.ResponseWriter, r *http.Request) {
	if allowed := e.methodsRegisteredForPath(r); len(allowed) > 0 {
		// The port requires Allow on every 405, and only the engine knows
		// the path's other methods, so it is set here rather than left to
		// the chain. It must precede the chain: the chain writes the status,
		// and a header set after the status line never reaches the client.
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		if len(e.noMethod) == 0 {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		run(e.noMethod, w, r)
		return
	}
	if len(e.noRoute) == 0 {
		http.NotFound(w, r)
		return
	}
	run(e.noRoute, w, r)
}

// probeMethods is the set re-asked of ServeMux to classify an unmatched
// request. It is every method net/http names; a route registered under some
// other verb is not reachable through this engine's own helpers anyway.
var probeMethods = [...]string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodConnect,
	http.MethodOptions, http.MethodTrace,
}

// methodsRegisteredForPath returns the methods other than the request's own
// that the path is registered under, which is both the 405 classification and
// the Allow header's value. Gin reports them in route-registration order; this
// engine reports them in probeMethods order, because ServeMux exposes no
// registration order to recover. Callers must treat Allow as a set.
func (e *Engine) methodsRegisteredForPath(r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := e.mux.Handler(probe); pattern != "" && pattern != unmatchedPattern {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

func run(chain []web.Handler, w http.ResponseWriter, r *http.Request) {
	rc := newRequestContext(chain, w, r)
	rc.Next()
	rc.commit()
}

var (
	_ web.EngineFactory = Factory{}
	_ web.Engine        = (*Engine)(nil)
	_ http.Handler      = (*Engine)(nil)
)
