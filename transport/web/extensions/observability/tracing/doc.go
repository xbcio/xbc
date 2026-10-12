// Package tracing provides the HTTP server middleware for XBC's
// application-private OpenTelemetry tracing.
//
// The tracing pipeline itself -- configuration bound from plugins.tracing, the
// private TracerProvider, the OTLP exporter, the W3C propagator, and the
// Handle contract -- belongs to the protocol-neutral plugin at
// github.com/xbcio/xbc/extensions/observability/tracing. This package only
// opens one server span per HTTP request on that pipeline: it extracts the
// incoming W3C context, names the span after the frozen route template, records
// the response status and any panic, and writes the configured trace and span
// correlation response headers.
//
// # Usage
//
// Configure plugins.tracing and select this package's Bundle, which includes
// the neutral tracing plugin. The middleware has no configuration of its own,
// activates on the same section, and runs in the observation phase ahead of
// metrics and access logging, so those middlewares observe a request that
// already carries an active span:
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			tracing.Bundle(),
//		))
//	}
//
// A plugins.tracing section activates both the pipeline and the middleware:
//
//	plugins:
//	  tracing:
//	    service_name: "orders"
//	    exporter:
//	      endpoint: "otel-collector:4317"
//
// A handler continues the request span through its context. Application code
// that records spans or propagates context outside HTTP handlers depends on
// the neutral plugin's Handle contract directly.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/web/extensions/observability/tracing/autoload;
// applications with a private catalog can use Definition directly.
//
// # Shutdown
//
// The middleware holds no resources and implements no lifecycle stage. It
// constructs after the neutral tracing plugin and before the Web server, so it
// is released before the plugin flushes and shuts its exporter down; see that
// package's Shutdown section.
package tracing
