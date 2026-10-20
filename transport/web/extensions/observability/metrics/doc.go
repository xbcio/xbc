// Package metrics provides the built-in HTTP server instrumentation and the
// optional Prometheus exposition endpoint for XBC.
//
// The registry and its configuration -- the plugins.metrics section, the
// private collector registry, and the Registerer contract -- belong to the
// protocol-neutral plugin at
// github.com/xbcio/xbc/extensions/observability/metrics. This package only
// instruments the Web server: it records bounded-cardinality request counters,
// a duration histogram, and an in-flight gauge per frozen route template,
// registers those three instruments with the neutral plugin's registry so one
// endpoint serves them beside application collectors, and contributes the
// scrape route on the management plane when plugins.metrics.http enables it.
//
// The process-level admission gate is exported as three unlabelled series:
// http_admission_limit (the effective in-flight ceiling),
// http_admission_in_flight (requests currently holding an admission slot), and
// http_admission_rejections_total (requests refused with the 503
// server_overloaded Problem). The gate refuses excess traffic before any
// contributed middleware runs, so the request instruments never observe those
// refusals; the admission series are what keep a saturated process visible in
// the same scrape instead of making it look idle. All three come from one
// snapshot per gather, and their names carry the configured namespace and
// subsystem exactly like the request instruments.
//
// # Usage
//
// Configure plugins.metrics and select this package's Bundle, which includes
// the neutral metrics plugin. The instrumentation has no configuration of its
// own, activates on the same section, and runs in the observation phase ahead
// of access logging:
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			metrics.Bundle(),
//		))
//	}
//
// The exposition endpoint is disabled by default; a plugins.metrics.http
// section enables it:
//
//	plugins:
//	  metrics:
//	    http:
//	      enabled: true
//	      path: "/metrics"
//
// Business collectors register with the neutral plugin's Registerer contract
// and appear in the same exposition; import the neutral package to declare
// that input.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/web/extensions/observability/metrics/autoload;
// applications with a private catalog can use Definition and
// AdmissionDefinition directly (Bundle selects both together).
package metrics
