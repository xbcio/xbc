// Package web provides XBC's optional engine-neutral HTTP transport.
//
// Web owns the HTTP server Plugin, route registration, domain-specific
// middleware and error-mapper ordering, and the immutable route catalog. Its
// surface names no HTTP engine: the engine arrives as a selected engine Bundle,
// and engine-specific capabilities are reached through that adapter's escape
// hatch rather than through this package.
//
// # Composition
//
// Ordinary imports are side-effect free. Explicit Bundle composition is the
// primary application API:
//
//	app, err := xbc.New(xbc.WithBundles(
//		webprelude.Bundle(),
//		ginengine.Bundle(),
//		orders.Bundle(),
//	))
//
// Web's Bundle contains the server, but no engine: exactly one engine Bundle
// must be selected alongside it, and transport/web/engines/gin is the one this
// repository ships. The prelude contributes the lightweight production
// baseline: request IDs, access logging, security headers, gzip, cooperative
// request timeouts, and health probes. Contributing is not enabling: the server
// and the health probes are active as soon as the Bundle is selected, while
// each remaining member stays dormant until its plugins.<key> section exists in
// the merged configuration, from a file or from the environment. The starter
// beside it is the policy half of that same baseline: it selects the prelude's
// Bundles and contributes the lowest configuration layer that switches those
// dormant members on, so a service obtains the baseline without shipping a
// file that restates it. Business response envelopes, CORS, authentication,
// authorization, persistence, telemetry exporters, and API documentation
// remain explicit Bundles because they select application policy or require
// optional dependencies.
//
// Executables that deliberately prefer process-global composition can blank
// import the leaf transport/web/autoload adapter and call xbc.Run. Prelude and
// implementation packages themselves never register from init.
//
// # Process scope
//
// A process hosts one App. Three owners make that a constraint rather than a
// preference, and none of them can be made per-App without giving up the thing
// it exists for: the selected engine adapter installs the engine's own
// process-global diagnostics, because constructing one points Gin's writers at
// the framework logger and picks Gin's mode from it, and no engine-neutral API
// can express either; the runtime installs the xbc.runtime processor and memory
// knobs, which describe the process rather than an App; and the log facade's
// global logger is the one every L() caller in the process reaches. A second
// App in one process would repoint the first App's engine diagnostics and
// overwrite its knobs while both kept serving.
//
// The constraint is narrow: everything else is per-App. Two Apps constructed in
// one process own their own plugin values, lifecycle contexts, task scopes, and
// servers, and their shutdowns do not cancel each other.
//
// # Contributions
//
// A Plugin contributes routes by exporting RouteContributor and contributes one
// independently ordered middleware by exporting Middleware. Factories receive
// dependencies through typed plugin input tokens; the server receives complete
// []plugin.Entry[T] collections at construction. There is no lifecycle-time
// capability scan or provider slice.
//
// A route contributor registers metadata beside each handler:
//
//	func (*Greeter) RegisterRoutes(router *web.Router) {
//		router.GET("/hello", func(ctx context.Context, c *web.Ctx) error {
//			c.String(http.StatusOK, "hello")
//			return nil
//		}).Name("greeting").Auth(web.Public())
//	}
//
// A route can carry plugin-owned data beside its handler with Route.Meta. The
// entry is written to every row the handle covers -- all of Any's or Match's
// methods, and every row of a mount -- and is readable, typed, from any frozen
// snapshot through RouteMeta[T], which asserts the value's exact dynamic type.
// The map is copied when the table is frozen and on every read, so a caller
// cannot mutate the dispatcher's copy; values are not deep-copied and must be
// treated as immutable from the moment Meta returns. There is no group-level
// default: unlike Perm and Auth, metadata is per-registration data. The startup
// route table prints the pairs, sorted by key, which also means route metadata
// must not carry credentials or other secrets.
//
// Route access is decided by a three-tier precedence model. An explicit
// web.security policy rule (tier 1) overrides a route's own .Auth()
// declaration (tier 2), and the global web.security.default (factory setting
// deny) covers routes that neither tier addresses. Auth(Public()) is a tier-2
// declaration; only a tier-1 rule can override it. The global default never
// overrides a route-level declaration. Omitting Auth leaves the route to the
// global default, Auth(Public()) explicitly bypasses authentication, and
// Auth(Accepts(...)) selects explicit schemes. The frozen RouteInfo.Auth
// pointer preserves those states; absence is never interpreted as public
// access.
//
// Route.Perm declares an authorization permission, but the framework does not
// enforce it by itself. A contributed middleware that enforces the permission a
// route declares implements RouteAuthorizer, and startup type-asserts every
// entry of the ordered middleware chain for that marker. When serving routes
// declare Route.Perm and no entry implements it, freezing logs one warning
// carrying the route count and up to three sample routes;
// web.security.require_route_authorizer turns that warning into a startup
// failure before any RouteCatalogListener is notified. The key defaults to
// false, so an existing composition keeps booting. Selecting the casbin-http
// plugin satisfies the check in either of its request conventions, while the
// protocol-neutral rbac adapter deliberately does not: its handler-scoped
// guards never read Route.Perm.
//
// Registration itself is validated before traffic opens. An Authenticator and
// a CredentialExtractor are separate plugin exports, so startup cross-checks
// them: a registered scheme with no extractor fails, naming the scheme and the
// plugins on both sides, because such a service could only answer requests
// selecting it with a generic 500. The remedy is either for the plugin to
// export both halves or for the application to select the transport's
// extractor adapter bundle for that scheme -- the jwt-http adapter beside the
// neutral jwt authenticator is one example. An extractor no registered
// authenticator uses is reported as a warning instead, since it may be shared
// or prepared for another manager. A registered scheme must also be
// reachable: it is named by a tier-1 policy rule, or selected by the effective
// policy of a route no rule covers, including a tier-2 Auth(Accepts(...))
// declaration. Under the permit default a scheme that neither of these names
// fails startup rather than sitting registered but unreachable.
//
// A contributor may also mount a handler that arrives with its own routing --
// an integration library, a file server, another framework's engine -- with
// Router.Mount, which registers the path as a subtree on every method and
// hands that handler the request with the prefix stripped. The subtree is an
// ordinary part of the same ingress: the same engine answers it on the same
// listener under the same base path, global middleware runs ahead of the
// mounted handler, and its requests resolve to the mount's own rows in the
// frozen table, so web.security covers the whole subtree exactly as it covers
// anywhere else -- a rule has to name the prefix itself ("/flow/**"), because
// a path inside the prefix has no row to match. Paths inside the prefix that
// the handler does not recognize are the handler's own 404; the framework's
// unmatched-request chains still answer every request the matcher never
// routed, which for a mount means the methods outside the covered set, as the
// usual 405 with Allow. Mount is not a plugin: it declares no configuration
// section, owns no lifecycle, and opens no listener, and constructing and
// mounting the handler has to be safe under Preflight, which performs the
// same route registration without ever binding one.
//
// Middleware identity is the producing plugin.Entry Identity. Phase is a hard
// outer-to-inner boundary, while typed Before and After references refine
// domain execution order. Missing preferred targets are reported; missing
// required targets, phase contradictions, and cycles fail startup. Middleware
// implementing authentication.RequiresPrincipal is framework-pinned after the
// canonical authentication middleware.
//
// The Server assembles three middleware stages itself rather than selecting
// them as plugins: the outermost panic boundary, the outermost error
// boundary, and the authentication middleware. A framework-owned ordering pin
// makes each one required, and a required stage must not be omittable at the
// composition root or disableable through plugins.<key>.enabled -- without the
// panic boundary a handler panic escapes every ordered middleware and reaches
// only net/http's own per-connection recover, which closes the connection
// without a response; without the error boundary every contributed
// ErrorMapper is unreachable; and without authentication every route is
// served unauthenticated even though web.security defaults to deny. Their
// identities are therefore reserved -- see ReservedMiddlewareKeys -- and a
// contributed Middleware claiming a reserved key fails startup. Ordering
// against them, with Require(AuthenticationMiddlewareKey) or by sitting
// inside PhaseError, is the supported use of those keys.
//
// Ahead of all of that, and outside the middleware chain entirely, sits the
// in-flight admission gate described under Configuration. It is not a
// Middleware and cannot be ordered against: an admission ceiling that an
// application could leave out of the chain would not be a ceiling. The
// in-flight gate, the request-body cap, and the error resolver attachment are
// the three framework stages that precede even the panic boundary; a panic
// inside one of them is not recovered by PhaseRecover, because PhaseRecover is
// a middleware and those three run before any middleware at all. That is a
// narrower exposure than it may read as: none of the three runs application
// or plugin code, so a panic there is a framework bug rather than a surface
// this boundary exists to cover.
//
// ErrorMapper Plugins declare a separate ErrorOrder. The Server collects and
// sorts all mapper entries into that boundary, which is pinned outermost in
// PhaseError. The first mapper that recognizes an error wins; safe non-leaking
// built-in mappings remain the fallback.
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
// Server.Preflight is the same assembly without the activation: it builds and
// registers the pipeline, freezes the route table, and prints the report a boot
// prints, but binds no listener and admits no serving task. The runtime invokes
// it only for the validate command, so a deployment can be checked -- by a
// process that exits -- before one that serves it is started.
//
// When runtime cancellation begins, Stop optionally leaves a serving listener
// up for web.shutdown.pre_drain_delay before calling http.Server.Shutdown. The
// default is 0s. A nonzero delay lets readiness probes receive 503 before HTTP
// draining begins, but consumes the shared runtime shutdown budget, continues
// to accept ordinary traffic, and is not acknowledgement that a load balancer
// has withdrawn the instance.
//
// # Management plane
//
// Routes contributed through Router.Management are served on a second engine.
// With web.management.addr empty -- the default -- they stay on the serving
// listener and its policy. Configuring an address binds a second listener whose
// chain carries only the panic boundary: no authentication, no admission gate,
// and no contributed middleware. A hostless address binds loopback, and a
// non-loopback address requires management.allow_remote, web.tls.cert_file, and
// management.client_ca_file. With a client CA configured, the management
// listener requires and verifies a client certificate from a TLS configuration
// cloned from the serving one -- certificate rotation and ALPN still apply --
// while business traffic keeps whatever web.tls.client_auth declares. There is
// no token option and no switch that turns the client-certificate requirement
// off. The management plane drains after the serving plane and shares its
// shutdown deadline.
//
// # Listener provider
//
// A plugin may export ListenerProvider to supply the sockets both planes serve
// through, replacing the net.Listen("tcp", address) call Start would make. It
// is an optional single export: with none selected the TCP behavior and its
// messages are unchanged, and two exporters fail assembly rather than letting
// boot order choose. Start calls Listen at most once per configured plane --
// the serving plane with web.addr, then the management plane with its resolved
// address -- and no other path calls it: Preflight, validate, doctor, and Stop
// never bind.
//
// TLS is wrapped above whatever the provider returns, so a PROXY-protocol
// provider must consume its header before the TLS records begin, and the
// listener it returns must be unstarted: the server owns it for the life of
// the plane and closes it. web.addr remains required and non-empty because
// configuration is validated before plugin inputs exist; its host:port shape is
// not validated, and the TCP path reports a malformed address at bind time.
// With a provider it is advisory, so a provider that binds something other
// than TCP keeps a plausible placeholder (the default ":8080" is one) and
// reads its real address from its own configuration section. Server.Addr and
// Server.ManagementAddr report the bound listener's address, which for a Unix
// socket is its path.
//
// # Configuration and proxy trust
//
// Config is bound from the root-level "web" section. Its defaults provide
// finite server timeouts and request-size limits:
//
//	web:
//	  addr: ":8080"
//	  base_path: "/api/v1"
//	  max_in_flight: 0
//	  shutdown:
//	    pre_drain_delay: 0s
//	  read_timeout: 10s
//	  read_header_timeout: 5s
//	  write_timeout: 30s
//	  idle_timeout: 60s
//	  max_header_bytes: 1048576
//	  max_request_body_bytes: 10485760
//	  max_request_body_bytes_ceiling: 0
//	  max_multipart_memory: 8388608
//	  trusted_proxies: []
//	  recovery:
//	    stack: true
//
// MaxRequestBodyBytes is the limit on the complete request body for a route that
// declares none, and MaxRequestBodyBytesCeiling is the bound no route can
// exceed. A route declares its own limit with Route.MaxRequestBodyBytes and is
// held to the smaller of that and the ceiling, so one upload route can take 200
// MiB while every other route stays at 10 MiB. The ceiling is unset by default
// and then equals MaxRequestBodyBytes: a deployment that never set it keeps
// exactly the bound it had, and a body larger than max_request_body_bytes is
// admitted only after the operator raises the ceiling, never as a side effect of
// a declaration in code. A ceiling below MaxRequestBodyBytes fails startup.
// MaxMultipartMemory only controls how much memory multipart parsing may use
// before spilling to temporary files; it does not replace the total body limit.
//
// MaxInFlight is the process-wide admission ceiling. A request beyond it is
// refused with 503 and a Retry-After header before any handler or contributed
// middleware runs, rather than being queued. It is a process property and never
// a per-route or per-plugin one, because a per-plugin ceiling cannot bound the
// sum an instance accepts. Zero, the default, applies a fixed default of 1024
// concurrent requests that is deliberately independent of the processor count:
// a request waiting on a database or an upstream holds no processor, so an
// IO-bound instance can serve far more requests than it has cores, and a
// CPU-derived ceiling would refuse traffic it could comfortably handle. Size the
// key from the deployment's own memory and dependency budget where the default
// is wrong for it.
//
// A route can be exempted from that ceiling with Route.Unmetered, which is what
// the health probes use: an orchestrator cannot distinguish "saturated" from
// "dead" unless the probe answers, and restarting a process at its traffic
// ceiling only moves that traffic to replicas that fail the same way. The gate
// recognizes the exemption by looking the request's own method and path up in
// the frozen route table -- never by asking the engine to resolve the route,
// because refusing before any handler runs is the property the gate exists for
// -- so an unmetered path must be a literal one and startup rejects a pattern.
// A route that takes the exemption is bounded by nothing but its own checks.
//
// TrustedProxies is empty by default, so forwarded headers such as
// X-Forwarded-For cannot influence the engine's client address. Configure only
// exact proxy IP addresses or CIDRs when the application is behind known reverse
// proxies. Never trust 0.0.0.0/0 or ::/0, and require the upstream proxy to
// remove client-supplied forwarding headers before adding its own.
//
// # HTTP error contract
//
// Built-in 404, 405, 413, timeout, panic, authentication, authorization, and
// binding or validation failures use RFC 9457 application/problem+json.
// ProblemDetail contains the five standard members and carries a stable
// application code as a top-level extension. It is an HTTP output model, not a
// base type for domain errors.
//
// The responses the framework generates itself -- 404, 405 and 413 -- travel
// through the complete global middleware chain rather than answering ahead of
// it, so they are access-logged, carry a request id, and receive CORS and
// security headers like any other response. A request whose path differs from
// a registered route only by a trailing slash is one of those unmatched
// paths: it receives the 404 Problem Detail through the chain rather than an
// engine-owned redirect, which would answer ahead of every stage above. For
// 413 that also means a protected route answers an unauthenticated oversized
// request with 401: the body limit is the innermost framework stage, behind
// authentication.
//
// Handle and AbortError resolve failures through the active mapper chain, which
// also receives errors the engine adapter collected from native middleware
// reporting through the engine's own error accumulator. The first resolved
// mapper that recognizes an error wins. Unknown errors become a fixed,
// non-leaking 500 response, context deadlines become 504, and an error cannot
// masquerade as HTTP 200. Plugins can contribute a nested boundary with
// OnError; panic recovery remains an independent outer concern.
//
// # Failure body rendering
//
// A plugin may export ErrorRenderer to replace the body of every failure
// response the Server writes. At most one renderer may be selected; with none,
// the default RFC 9457 Problem Detail above is unchanged byte for byte. The
// renderer decides the body only: it receives the request context and the
// normalized ProblemDetail (type, title and instance filled in, status inside
// 100-599), and returns the body. The status and the headers stay the
// framework's -- including no-store, nosniff and the removal of stale
// Content-Encoding or Content-Length -- so a renderer cannot change a failure's
// HTTP semantics; only the Content-Type becomes application/json. Returning
// ok=false or an empty body declines, and that response is the default Problem
// Detail. A renderer that panics is contained: the framework logs the panic,
// and that response is the default Problem Detail too, with the failure's own
// status -- but a renderer must not rely on that containment.
//
// The renderer is consulted from WriteProblem, the one point every failure
// leaves through, so it covers mapped errors, the panic boundary, the
// authentication 401, the oversized-body 413 and the framework's own 404 and
// 405 alike, and it applies inside OnError scopes as well. It is never
// consulted for a response that is already committed, and never for a success.
// Two answers keep the Problem Detail whatever renderer is selected,
// deliberately rather than by oversight. The management listener runs its own
// short chain with no error resolver attached, so its 404 and 405 are always
// Problem Details. The in-flight admission gate refuses an overloaded request
// ahead of every other stage, before the resolver exists, so its 503 is one
// too.
//
// # Streaming responses
//
// A handler that produces its response incrementally calls Ctx.BeginStream,
// which sets the content type, commits the status, and returns a StreamWriter
// that flushes after every write; Ctx.SSE begins a text/event-stream response
// with the headers an event stream needs and returns an encoder for events and
// keep-alive comments. Beginning the stream commits the response, so a later
// handler error can no longer be rendered and must be sent as an error event
// instead. BeginStream lifts the connection's write deadline for that response
// only, so a stream is not cut by web.write_timeout while ordinary requests
// keep their bound. It does not lift the timeout plugin's context deadline:
// once a stream begins that plugin abandons its 504, but the handler's context
// is still cancelled at plugins.timeout.duration unless the request carries
// "Accept: text/event-stream" or an Upgrade, or the path or route is excluded
// from the middleware, so a streaming handler must treat context cancellation
// as its own stop signal. Streaming and the idempotency plugin do not combine:
// idempotency captures a bounded response body and replays it, and a stream
// cannot be captured, so a route must not enable both.
//
// # Request binding and validation
//
// Use Ctx.Bind or Ctx.BindURI, which already adapt failures through ParamError.
// They are the correct entry point because the neutral surface defines binding
// as an action rather than a binder catalogue: the engine adapter performs it
// and normalizes the failure before the error boundary sees it. For a binder Ctx
// does not expose, take the native context from the selected engine adapter's
// escape hatch -- ginengine.FromCtx(c) for the Gin adapter -- and pass the
// resulting error to ParamError yourself:
//
//	router.POST("/orders", func(ctx context.Context, c *web.Ctx) error {
//		var request CreateOrderRequest
//		if err := c.Bind(&request); err != nil {
//			return err
//		}
//		c.JSON(http.StatusCreated, createOrder(ctx, request))
//		return nil
//	})
//
// Do not use an engine's own Bind or MustBind families: they write a 400
// response before the centralized error boundary can handle the failure.
// ParamError adapts an existing error; it does not read the body, choose a
// binder, or run validation again. Validation responses expose a public field
// name, a constraint code, and a safe message without echoing rejected values.
//
// DTO struct tags are the one place where the engine choice does leak into
// application code, and the leak is deliberate rather than papered over: the
// neutral surface defines the binding action, not the tag language, so binding
// and validation tags follow the selected engine. Rejecting unknown JSON fields,
// and supporting additional formats, are configured through that engine's own
// extension points rather than through a parallel binding API in Web. Domain
// packages should remain transport-neutral and be adapted with ErrorMapper at
// the application boundary.
package web
