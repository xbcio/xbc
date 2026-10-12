// Package tracing provides the gRPC server interceptor for XBC's
// application-private OpenTelemetry tracing.
//
// The tracing pipeline itself -- configuration bound from plugins.tracing, the
// private TracerProvider, the OTLP exporter, the W3C propagator, and the
// Handle contract -- belongs to the protocol-neutral plugin at
// github.com/xbcio/xbc/extensions/observability/tracing. This package only
// opens one server span per RPC on that pipeline: it extracts the incoming W3C
// context from the call metadata, names the span after the full method, records
// the resulting status code and any panic, and writes the configured trace and
// span correlation metadata on the response.
//
// # Usage
//
// Configure plugins.tracing and select this package's Bundle, which includes
// the neutral tracing plugin. The interceptor has no configuration of its own,
// activates on the same section, and contributes to both the unary and the
// stream chain in the observation phase, ahead of metrics, so the sibling
// interceptors observe a call that already carries an active span:
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			grpc.Bundle(),
//			tracing.Bundle(),
//		))
//	}
//
// A plugins.tracing section activates both the pipeline and the interceptor:
//
//	plugins:
//	  tracing:
//	    service_name: "orders"
//	    exporter:
//	      endpoint: "otel-collector:4317"
//
// Correlation metadata is written before the handler runs, under the
// lower-cased forms of the configured header names (gRPC metadata keys are
// lower case), the gRPC counterpart of the Web interceptor's response
// headers. Server-side application code that records spans depends on the
// neutral plugin's Handle contract directly.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/grpc/extensions/observability/tracing/autoload;
// applications with a private catalog can use Definition directly.
//
// # Shutdown
//
// The interceptor holds no resources and implements no lifecycle stage. It
// constructs after the neutral tracing plugin and before the gRPC server, so
// it is released before the plugin flushes and shuts its exporter down; see
// that package's Shutdown section.
package tracing
