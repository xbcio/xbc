// Package grpc is XBC's gRPC transport: a server Plugin that collects the
// services and interceptors a composition contributes, freezes them before any
// listener opens, and serves them under the runtime's lifecycle and traffic
// gate.
//
// The transport is modelled on its own terms rather than as a variant of the
// Web transport, and the two never import each other. Services register through
// a Registrar that generated code accepts unchanged; unary and stream
// interceptors are separate contracts, each ordered by Phase and Before/After
// references and sorted per chain, with a cycle or a phase contradiction
// failing startup. Three stages lead both chains and are assembled by the
// Server, not contributed, so a composition root cannot leave them out: the
// panic boundary, the status boundary, and authentication.
//
// # Usage
//
// A plugin contributes services by implementing ServiceRegistrar and exporting
// it; generated registration functions take the Registrar directly:
//
//	func (p *Plugin) RegisterServices(r *xbcgrpc.Registrar) error {
//		greeterpb.RegisterGreeterServer(r, p.greeter)
//		return nil
//	}
//
//	definition = plugin.Define("greeter", newPlugin, plugin.Options[*Plugin]{
//		Exports: plugin.Contracts(
//			plugin.ExportAs[xbcgrpc.ServiceRegistrar](func(p *Plugin) xbcgrpc.ServiceRegistrar { return p }),
//		),
//	})
//
// and the composition root selects the server beside it:
//
//	xbc.Run(xbc.WithBundles(xbcgrpc.Bundle(), greeter.Bundle()))
//
// The server's Bundle is the server alone. The prelude package beside it
// (transport/grpc/prelude) contributes the lightweight production baseline --
// the server, the standard grpc.health.v1 service with the neutral health
// capability behind it, and structured access logging (plugins.grpc-accesslog)
// -- and, unlike Web's prelude, is startable on its own, because the gRPC
// server selects no engine. Contributing is not enabling: the server and the
// health service are active as soon as the Bundle is selected, while access
// logging stays dormant until a plugins.grpc-accesslog section exists in the
// merged configuration. The starter beside it (transport/grpc/starter) is the
// policy half of that same baseline: it selects the prelude's Bundles and
// contributes the lowest configuration layer that switches access logging on.
// Metrics, tracing, authentication schemes, authorization, TLS material, and
// business services remain explicit Bundles: the first two are independently
// versioned modules that bring Prometheus and OpenTelemetry into the
// dependency closure, and the rest select application policy.
//
// The server reads its root-level "grpc" section:
//
//	grpc:
//	  addr: ":9090"
//	  max_recv_msg_bytes: 4194304
//	  max_send_msg_bytes: 4194304
//	  max_concurrent_streams: 100
//	  shutdown:
//	    pre_drain_delay: 0s
//	  recovery:
//	    stack: true
//	  security:
//	    default: deny
//
// # Authentication
//
// Every method is denied unless something decides otherwise, in three tiers:
// a grpc.security rule outranks the registering plugin's declaration, which
// outranks grpc.security.default. A plugin declares a policy beside its
// registration with the Methods handle, at service or method level:
//
//	greeterpb.RegisterGreeterServer(r, p.greeter)
//	r.Methods("greeter.v1.Greeter").Auth(xbcgrpc.Accepts("jwt")).MethodAuth("Ping", xbcgrpc.Public())
//
// Rules match full method names -- "/pkg.Service/Method", "/pkg.Service/*",
// or "/**" -- first match wins, and a rule an earlier one makes unreachable
// fails startup. A method with no rule and no declaration falls to the
// default; no declaration never means public. The transport's probe and
// schema surfaces are reachable by declaration rather than by rule: the
// health-grpc plugin declares the standard probe Public, and reflection,
// when enabled, compiles its methods as Public -- both stay tightenable,
// because a grpc.security rule outranks a declaration (see Health and
// reflection).
//
//	grpc:
//	  security:
//	    default: deny
//	    schemes: [jwt, apikey]
//	    rules:
//	      - match: /admin.v1.Admin/*
//	        authenticate: [apikey]
//
// Authenticators are the protocol-neutral authentication.Authenticator
// plugins; each needs a CredentialExtractor that reads its credential from
// call metadata, which the scheme's gRPC Bundle provides (for example
// extensions/authentication/apikey or the jwt module beside it). With two or
// more authenticators, schemes must list every registered scheme in the order
// they are tried: arbitration order is a security decision and is never
// inferred. An unknown, missing, or repeated scheme, an authenticator without
// an extractor, or a method needing authentication with none registered fails
// startup.
//
// A rejected call ends with codes.Unauthenticated and the challenges of the
// tried schemes in the www-authenticate trailer. An authenticated call carries
// its Principal, read with PrincipalFromContext (on a stream, from
// ServerStream.Context()); a permitted call reports AuthenticationExempt. A
// contributed interceptor implementing authentication.RequiresPrincipal is
// pinned after the stage. A stream is authenticated once, when it opens.
//
// # Health and reflection
//
// The health-grpc plugin (transport/grpc/extensions/reliability/health)
// serves the standard grpc.health.v1.Health service from the runtime's probe
// state: "" and "readiness" report readiness -- NOT_SERVING until the traffic
// gate opens and again while the runtime drains -- "liveness" reports process
// liveness, an unregistered service answers codes.NotFound, and Watch answers
// codes.Unimplemented because the prober is a pull-only aggregator. The probe
// methods are declared Public so load balancers and orchestrators reach them
// without credentials.
//
// With grpc.reflection.enabled -- false by default, because it publishes the
// full service schema -- the server installs grpc-go's standard reflection
// service, both grpc.reflection.v1 and grpc.reflection.v1alpha, so tools like
// grpcurl can read the schema without generated stubs. Its methods compile as
// Public, since such tools show no credentials by default, and the startup
// report names the service and that fact.
//
//	grpc:
//	  reflection:
//	    enabled: true
//
// # TLS
//
// grpc.tls terminates TLS on the serving listener, with the same fields and
// reload behavior as the Web transport's TLS section: cert_file and key_file
// name the certificate pair on disk, min_version accepts "1.2" or "1.3",
// client_ca_file with client_auth ("verify_if_given" or "require_and_verify")
// turns on client-certificate verification, and cipher_suites restricts the
// TLS 1.2 suites by Go name (TLS 1.3 suites are not configurable in Go). An
// empty cert_file serves plaintext. With reload_interval set, the pair is
// checked for a change at most once per interval, at the next handshake: a
// failed reload keeps the previous certificate and reports the file state
// once. ALPN advertises h2, so gRPC clients negotiate HTTP/2.
//
//	grpc:
//	  tls:
//	    cert_file: /etc/xbc/tls/tls.crt
//	    key_file: /etc/xbc/tls/tls.key
//	    min_version: "1.3"
//	    client_ca_file: /etc/xbc/tls/ca.crt
//	    client_auth: require_and_verify
//
// # Errors
//
// grpc-go would answer a handler's non-status error with codes.Unknown and the
// error's own text. The status boundary instead passes a status error through,
// maps context cancellation and expiry to their codes, offers anything else to
// the StatusMapper plugins in their declared StatusOrder, and answers what none
// claims with codes.Internal "internal error", logging the cause server-side
// only. Authentication's operational failures likewise surface as a fixed
// codes.Internal "authentication failed".
//
// # Lifecycle
//
// Start collects every contribution into a frozen service table -- a service
// name registered twice, a nil or mistyped implementation, or a method declared
// twice fails startup with an error rather than reaching grpc-go's own checks,
// which terminate the process -- sorts both interceptor chains, binds the
// listener, and submits a critical task that waits for the runtime traffic gate
// before serving. OpenTraffic reports the listener, both chains, the method
// table, and every method's authentication decision with the tier that made
// it. Preflight, run by the validate command, performs the same assembly and
// report without binding a listener or constructing a server.
//
// # Shutdown
//
// The Server is ingress, so it implements Stop and no Drain. A server that
// served traffic waits shutdown.pre_drain_delay, then stops gracefully: it
// refuses new RPCs and lets in-flight ones finish for as long as the runtime's
// shutdown deadline allows. When the deadline ends first, the remaining RPCs are
// cancelled and Stop reports the expiry. A server that never crossed the
// traffic gate only closes its listener.
package grpc
