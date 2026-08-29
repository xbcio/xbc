// Package web provides xbc's optional Gin-backed HTTP transport.
//
// The package owns the HTTP server plugin, route registration, middleware
// ordering, and the frozen route catalog. It deliberately exposes Gin types:
// web is a concrete application runtime, not a protocol-neutral abstraction.
// Applications that do not enable it do not pull Gin into the xbc core module.
//
// # Enabling the server
//
// Importing web has no registration side effects. Plugin and library packages
// should import it normally when they implement RouteProvider,
// MiddlewareProvider, or RouteCatalogConsumer. An executable that uses xbc's
// process-wide default catalog enables the server explicitly through autoload:
//
//	import (
//		"github.com/xbcio/xbc"
//		_ "github.com/xbcio/xbc/transport/web/autoload"
//	)
//
//	func main() { xbc.Run() }
//
// A host that assembles a private plugin catalog should add Definition() to
// that catalog instead of importing autoload.
//
// # Routes
//
// Application plugins contribute routes by implementing RouteProvider:
//
//	type Greeter struct{}
//
//	var _ web.RouteProvider = (*Greeter)(nil)
//
//	func (*Greeter) RegisterRoutes(r *web.Router) {
//		r.GET("/hello", func(c *gin.Context) {
//			c.String(http.StatusOK, "hello")
//		})
//	}
//
// Server discovers every provider after plugin initialization, invokes them in
// dependency order, and then freezes the route table before traffic is
// accepted. Router.Group creates nested route groups, and all registered paths
// are relative to Config.BasePath. Routes cannot be added after the table has
// been frozen.
//
// # Middleware and route metadata
//
// MiddlewareProvider contributes named Gin handlers. Phase establishes the
// hard outer-to-inner order of the middleware chain; After and Before refine
// ordering within a phase and must not contradict phase order.
//
// RouteCatalogConsumer receives the complete, read-only RouteCatalog after all
// RouteProvider calls finish. Within a request handler or middleware,
// CurrentRoute reports the matching frozen RouteInfo, including its HTTP method
// and route-template path; it returns false for unmatched requests.
//
// # Configuration and lifecycle
//
// Config is bound below the plugin's stable key, "web":
//
//	plugins:
//	  web:
//	    addr: ":8080"
//	    base_path: "/api/v1"
//	    read_timeout: 10s
//	    write_timeout: 30s
//
// During xbc's first readiness phase, Server assembles the Gin engine, freezes
// the route table, and binds the listener without serving requests. Traffic is
// opened only after every application runner has started successfully. On
// shutdown, Server drains HTTP connections using the deadline supplied by the
// core runtime; the application-wide shutdown budget therefore belongs to
// xbc.shutdown_timeout, not to Config.
package web
