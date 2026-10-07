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
//	type slogSink struct {
//		logger *slog.Logger
//	}
//
//	func (s slogSink) Write(ctx context.Context, event auditlog.Event) error {
//		s.logger.LogAttrs(ctx, slog.LevelInfo, "http audit",
//			slog.String("method", event.Method),
//			slog.String("route", event.RouteTemplate),
//			slog.Int("status", event.Status),
//			slog.String("subject", event.Subject),
//		)
//		return nil
//	}
//
//	func newAuditPlugin(logger *slog.Logger) (*auditlog.Plugin, error) {
//		return auditlog.New(auditlog.DefaultConfig(), auditlog.WithSink(slogSink{logger: logger}))
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
// deployment's trusted-proxy policy. Bundle and ordinary imports are side-effect
// free. Applications that prefer xbc.Run may opt into process-global
// composition through this package's autoload leaf.
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
