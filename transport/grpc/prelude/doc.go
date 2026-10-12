// Package prelude provides XBC's lightweight, production-safe gRPC baseline as
// side-effect-free composition data. It is a pure aggregate: it declares no
// Definition and creates no runtime unit of its own.
//
// Its Bundle explicitly combines the member Bundles for the gRPC server --
// whose panic recovery, status mapping, and authentication stages, and
// optional reflection service, the server assembles itself -- the standard
// grpc.health.v1 service with the neutral health capability behind it, and
// structured access logging. Every member Definition keeps its own activation
// policy and configuration path; prelude neither clones declarations nor
// registers from init. Access logging is therefore dormant until a
// plugins.grpc-accesslog section exists; the starter package beside this one
// is what turns it on.
//
// Metrics and tracing are deliberately not members. Their interceptors are
// independently versioned modules
// (transport/grpc/extensions/observability/metrics and .../tracing) because
// they bring Prometheus and OpenTelemetry into the dependency closure, and
// this package belongs to the transport/grpc module, whose closure stays free
// of both. A composition root that wants them selects their Bundles beside
// this one and configures plugins.metrics and plugins.tracing. Authentication
// schemes, authorization, and business services are likewise left to the
// composition root, because each selects an application contract or policy.
//
// Unlike the Web prelude, this Bundle is startable on its own: the gRPC
// server has no separate engine to select.
//
// # Usage
//
//	app, err := xbc.New(xbc.WithBundles(
//		prelude.Bundle(),
//		greeter.Bundle(),
//	))
//
// Importing prelude never mutates process-global state.
package prelude
