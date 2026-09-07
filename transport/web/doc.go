// Package web provides XBC's optional Gin-backed HTTP transport.
//
// Web owns the HTTP server Plugin, route registration, domain-specific
// middleware and error-mapper ordering, and the immutable route catalog. It
// deliberately exposes Gin types: this module is a concrete transport runtime,
// not a protocol-neutral abstraction.
//
// # Composition
//
// Ordinary imports are side-effect free. Explicit Bundle composition is the
// primary application API:
//
//	app, err := xbc.New(xbc.WithBundles(
//		webprelude.Bundle(),
//		orders.Bundle(),
//	))
//
// Web's Bundle contains the server and its independently identified error
// boundary. The prelude adds the lightweight production baseline: recovery,
// request IDs, access logging, security headers, gzip, cooperative request
// timeouts, and health probes. Business response envelopes, CORS,
// authentication, authorization, persistence, telemetry exporters, and API
// documentation remain explicit Bundles because they select application policy
// or require optional dependencies.
//
// Executables that deliberately prefer process-global composition can blank
// import the leaf transport/web/autoload adapter and call xbc.Run. Prelude and
// implementation packages themselves never register from init.
//
// # Contributions
//
// A Plugin contributes routes by exporting RouteContributor and contributes one
// independently ordered middleware by exporting Middleware. Factories receive
// dependencies through typed plugin input tokens; the server receives complete
// []plugin.Entry[T] collections at construction. There is no lifecycle-time
// capability scan or provider slice.
//
// A route contributor registers metadata beside each Gin handler:
//
//	func (*Greeter) RegisterRoutes(router *web.Router) {
//		router.GET("/hello", func(c *gin.Context) {
//			c.String(http.StatusOK, "hello")
//		}).Name("greeting").Auth(web.Public())
//	}
//
// Route authentication has three distinct states. Omitting Auth selects the
// authentication manager's restrictive default, Auth(Public()) explicitly
// bypasses authentication, and Auth(Accepts(...)) selects explicit schemes.
// The frozen RouteInfo.Auth pointer preserves those states; absence is never
// interpreted as public access.
//
// Middleware identity is the producing plugin.Entry Identity. Phase is a hard
// outer-to-inner boundary, while typed Before and After references refine
// domain execution order. Missing preferred targets are reported; missing
// required targets, phase contradictions, and cycles fail startup. Middleware
// implementing security.RequiresPrincipal is framework-pinned after the
// canonical authentication middleware.
//
// ErrorMapper Plugins declare a separate ErrorOrder. The web-error-boundary
// Plugin consumes and sorts all mapper entries, exports Middleware, and is
// pinned outermost in PhaseError. The first mapper that recognizes an error
// wins; safe non-leaking built-in mappings remain the fallback.
//
// RouteCatalogListener receives the complete immutable catalog during traffic
// preparation, after every contributor has run and authentication metadata has
// validated. CurrentRoute exposes the matching frozen RouteInfo to handlers and
// middleware.
//
// # Lifecycle and traffic gate
//
// Server.Start assembles the pipeline, registers routes, binds the listener,
// and submits one managed critical serving task. That task waits for either the
// runtime-owned traffic gate or task cancellation before calling Serve.
// Server.OpenTraffic performs only fallible preparation: it freezes the route
// table and notifies listeners. It neither starts a task nor releases traffic.
// Runtime closes the single gate only after every Plugin's preparation succeeds,
// so one failure leaves every ingress blocked. Stop drains a serving server or
// closes a listener that never crossed the gate.
//
// # Configuration and proxy trust
//
// Config is bound from the root-level "web" section. Its defaults provide
// finite server timeouts and request-size limits:
//
//	web:
//	  addr: ":8080"
//	  base_path: "/api/v1"
//	  read_timeout: 10s
//	  read_header_timeout: 5s
//	  write_timeout: 30s
//	  idle_timeout: 60s
//	  max_header_bytes: 1048576
//	  max_request_body_bytes: 10485760
//	  max_multipart_memory: 8388608
//	  trusted_proxies: []
//
// MaxRequestBodyBytes is a hard limit for the complete request body.
// MaxMultipartMemory only controls how much memory multipart parsing may use
// before spilling to temporary files; it does not replace the total body limit.
//
// TrustedProxies is empty by default, so forwarded headers such as
// X-Forwarded-For cannot influence Gin's client address. Configure only exact
// proxy IP addresses or CIDRs when the application is behind known reverse
// proxies. Never trust 0.0.0.0/0 or ::/0, and require the upstream proxy to
// remove client-supplied forwarding headers before adding its own.
//
// # HTTP error contract
//
// Built-in 404, 405, 413, timeout, panic, authentication, authorization, and
// Gin binding or validation failures use RFC 9457 application/problem+json.
// ProblemDetail contains the five standard members and carries a stable
// application code as a top-level extension. It is an HTTP output model, not a
// base type for domain errors.
//
// Handle and AbortError resolve failures through the active mapper chain, which
// also receives errors reported through Gin Context.Error. The first resolved
// mapper that recognizes an error wins. Unknown errors become a fixed,
// non-leaking 500 response, context deadlines become 504, and an error cannot
// masquerade as HTTP 200. Plugins can contribute a nested boundary with
// OnError; panic recovery remains an independent outer concern.
//
// # Request binding and validation
//
// Use a Gin ShouldBind method and adapt failures with ParamError; Web does not
// duplicate Gin's binder APIs:
//
//	router.POST("/orders", web.Handle(func(c *gin.Context) error {
//		var request CreateOrderRequest
//		if err := c.ShouldBindJSON(&request); err != nil {
//			return web.ParamError(err, &request)
//		}
//		c.JSON(http.StatusCreated, createOrder(request))
//		return nil
//	}))
//
// Do not use Gin's Bind or MustBind families: they write a 400 response before
// the centralized error boundary can handle the failure. ParamError adapts an
// existing error; it does not read the body, choose a binder, or run validation
// again. Validation responses expose a public field name, a constraint code,
// and a safe message without echoing rejected values.
//
// Applications that reject unknown JSON fields must explicitly enable Gin's
// decoder option during startup. Additional formats should implement Gin's
// binding.Binding or binding.BindingBody extension points rather than adding a
// parallel binding API to Web. Domain packages should remain transport-neutral
// and be adapted with ErrorMapper at the application boundary.
package web
