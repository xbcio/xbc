// Package requestid validates or generates request IDs, publishes them on the
// response, propagates them through Ctx and standard request contexts, and --
// by default -- binds them to the request context's logger.
//
// # Usage
//
// Compose request-ID middleware explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		requestid "github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			requestid.Bundle(),
//		))
//	}
//
// A plugins.requestid section activates the composed Definition. Public
// services commonly generate a server-owned ID rather than trusting inbound
// values:
//
//	plugins:
//	  requestid:
//	    header: X-Request-ID
//	    trust_incoming: false
//	    bind_logger: true
//	    max_length: 128
//
// Handlers can read the validated ID off Ctx and pass the request context to
// downstream work. FromRequest and FromContext provide the same value to code
// that does not depend on Ctx:
//
//	func submit(ctx context.Context, c *web.Ctx) error {
//		id, ok := requestid.From(c)
//		if !ok {
//			return errors.New("request id unavailable")
//		}
//		log.Printf("request %s", id)
//		doWork(c.Request().Context())
//		c.Status(http.StatusAccepted)
//		return nil
//	}
//
// Missing, duplicate, malformed, or oversized inbound IDs are replaced. An
// accepted inbound ID is validated -- one value, an allowed character set, at
// most max_length bytes -- but it is still chosen by the client. A reverse
// proxy in front of the service must therefore strip or overwrite the inbound
// header; a deployment without such a trusted proxy should set
// trust_incoming: false so every ID is generated server-side. The middleware
// runs before accesslog so access records can safely consume the validated
// response header. Bundle and ordinary imports are side-effect free.
// Applications that prefer xbc.Run may opt into process-global composition
// through this package's autoload leaf.
//
// # Logging
//
// With bind_logger enabled (the default), the middleware publishes the ID on
// the request context's logger, so business code that logs through the facade
// records it without any extra plumbing:
//
//	func submit(_ context.Context, c *web.Ctx) error {
//		log.Ctx(c.Request().Context()).Info("order accepted", "order_id", id)
//		c.Status(http.StatusAccepted)
//		return nil
//	}
//
// The record carries request_id with the same value as the response header
// the middleware wrote. FromContext keeps working unchanged: the ID still
// travels as a plain request-context value. Binding happens before the rest of
// the chain runs, so middleware and handlers that log -- or panic -- after
// this point already carry the field. A second pass over one request reuses
// the ID decided by the first pass and does not add the field twice.
// bind_logger: false leaves the request context's logger untouched; the ID is
// still propagated through Ctx and the request context, and still published
// on the response.
//
// # Relationship to tracing
//
// When HTTP tracing is composed it runs ahead of this middleware, so the
// request context already carries a trace; the bound logger then records the
// trace fields (trace_id, span_id, span_name) as well. request_id and trace_id
// remain separate fields with different jobs: request_id identifies this HTTP
// exchange and joins the access log (plugins.accesslog) to business records,
// while trace_id reaches the spans recorded in a tracing backend. The
// middleware makes the request ID authoritative for the hop -- the trace's own
// request_id, a ULID derived from trace_id, is replaced rather than logged
// beside it -- but it never forces request_id and trace_id to be equal,
// because one trace can cover several requests. Filter by request_id to
// diagnose a single exchange; follow trace_id to see the surrounding call
// chain.
//
// The access log takes request_id from the response header this middleware
// writes, and the bound logger fixes the same value, so both views of a
// request correlate on one ID. Ordering is part of that contract: requestid
// runs after optional HTTP tracing (to see its trace) and before access
// logging (to publish the header it consumes).
package requestid
