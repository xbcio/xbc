// Package prelude provides XBC's lightweight, production-safe Web baseline as
// side-effect-free composition data.
//
// The Bundle combines the HTTP server plus recovery, request IDs, structured
// access logging, security headers, compression, cooperative request timeouts,
// and health probes. It preserves every member's canonical Definition,
// activation policy, and configuration path; it neither clones declarations nor
// registers from init.
//
// Business response envelopes, CORS, authentication, authorization,
// persistence, telemetry exporters, and API documentation are deliberately not
// included because they select an application contract or policy, or require
// an optional third-party dependency.
//
// # Usage
//
// Explicit composition is the primary API:
//
//	app, err := xbc.New(xbc.WithBundles(
//		prelude.Bundle(),
//		orders.Bundle(),
//	))
//
// Executables that deliberately prefer xbc.Run may blank import the optional
// transport/web/autoload leaf adapter. Importing prelude itself never mutates
// process-global state.
package prelude
