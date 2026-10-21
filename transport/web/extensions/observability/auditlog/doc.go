// Package auditlog records bounded HTTP audit metadata without capturing
// request or response bodies. It consumes the shared Web Principal contract
// and therefore does not depend on JWT, API-key, or another authentication
// implementation.
//
// # Usage
//
// The default plugin writes structured events through XBC's logger. A private
// assembly can inject a Sink for another audit destination; the sink receives
// metadata only and must honor context cancellation:
//
//	type facadeSink struct{}
//
//	func (facadeSink) Write(ctx context.Context, event auditlog.Event) error {
//		log.Ctx(ctx).Info("http audit",
//			"method", event.Method,
//			"route", event.RouteTemplate,
//			"status", event.Status,
//			"subject", event.Subject,
//		)
//		return nil
//	}
//
//	func newAuditPlugin() (*auditlog.Plugin, error) {
//		return auditlog.New(auditlog.DefaultConfig(), auditlog.WithSink(facadeSink{}))
//	}
//
// Compose the Bundle explicitly with the application's other Bundles:
//
//	app, err := xbc.New(xbc.WithBundles(
//		webprelude.Bundle(),
//		ginengine.Bundle(),
//		auditlog.Bundle(),
//	))
//
// Event cannot represent request bodies, response bodies, or an idempotency-key
// value. The default client-IP extractor reads RemoteAddr and ignores spoofable
// forwarding headers; inject WithClientIPExtractor only when it enforces the
// deployment's trusted-proxy policy. The default request-ID extractor reads
// the response header written by the requestid middleware, so the event
// carries the same validated ID the framework wrote and the access log
// recorded. Without requestid composed the field is empty: an inbound header
// is never promoted to the framework request ID, because its value is
// client-chosen. An application whose request ID lives elsewhere can inject
// WithRequestIDExtractor; request_id_header must match plugins.requestid.header
// when that setting is customized, since both name the same response header.
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
//
// With async dispatch, shutdown is split in two: Drain runs after the Web
// server has stopped serving and writes every queued event to the sink within
// the drain budget; Stop then drains what is left within the caller's shutdown
// deadline, cancels -- rather than waits for -- a worker that deadline expires
// on, and attempts one final flush. The flush is capped at sink_timeout but
// never outlives the caller's deadline, so a deadline the drain already spent
// leaves it a best-effort call the sink is expected to refuse through the
// cancelled context. Every individual sink write is bounded by sink_timeout
// too.
package auditlog
