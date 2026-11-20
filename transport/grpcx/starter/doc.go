// Package starter provides XBC's product-level gRPC baseline: one value that
// selects the gRPC prelude and turns on the capabilities in it that are
// dormant by default.
//
// The prelude contributes its Definitions as side-effect-free composition
// data and changes none of their activation policies, so on its own it
// delivers a gRPC server and the standard health service. This package adds
// the policy half: its configuration layer is merged below every other source
// and switches on structured access logging (plugins.grpc-accesslog). The
// server, its panic, status, and authentication stages, and the health
// service are active as soon as their Bundle is selected and need no entry
// here.
//
// # What it deliberately leaves alone
//
// Reflection stays off (grpc.reflection.enabled), because it publishes the
// full service schema. Metrics and tracing are not selected: they are
// independently versioned modules that bring Prometheus and OpenTelemetry
// with them, so the composition root selects their Bundles beside the
// starter. Authentication schemes, authorization, TLS material, and every
// capability's own configured values stay with the application or with the
// owning plugin's declared defaults; this starter decides only which prelude
// members are on.
//
// # Usage
//
//	xbc.Run(
//		xbc.WithStarter(grpcstarter.GRPC()),
//		xbc.WithBundles(greeter.Bundle()),
//	)
//
// Everything the starter contributes is an ordinary lowest-precedence layer,
// so a configuration file, a profile overlay, an Override, or an environment
// variable can disagree with it -- including turning access logging back off
// with plugins.grpc-accesslog.enabled: false.
package starter
