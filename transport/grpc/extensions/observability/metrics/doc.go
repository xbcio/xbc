// Package metrics provides the built-in gRPC server instrumentation for XBC.
//
// The registry and its configuration -- the plugins.metrics section, the
// private collector registry, the duration buckets, and the Registerer
// contract -- belong to the protocol-neutral plugin at
// github.com/xbcio/xbc/extensions/observability/metrics. This package only
// instruments the gRPC server: it records bounded-cardinality call counters, a
// duration histogram, and an in-flight gauge per service and method, and
// registers those three instruments with the neutral plugin's registry so one
// exposition serves them beside application collectors (the Web
// instrumentation's endpoint, when plugins.metrics.http enables it).
//
// # Usage
//
// Configure plugins.metrics and select this package's Bundle, which includes
// the neutral metrics plugin. The instrumentation has no configuration of its
// own, activates on the same section, and runs in the observation phase, after
// the tracing interceptor has opened the call's span:
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			grpc.Bundle(),
//			metrics.Bundle(),
//		))
//	}
//
// A plugins.metrics section activates both the registry and the
// instrumentation:
//
//	plugins:
//	  metrics:
//	    namespace: "orders"
//
// Business collectors register with the neutral plugin's Registerer contract
// and appear in the same exposition; import the neutral package to declare
// that input.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/grpc/extensions/observability/metrics/autoload;
// applications with a private catalog can use Definition directly.
package metrics
