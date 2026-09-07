// Package requestid validates or generates request IDs, publishes them on the
// response, and propagates them through both Gin and standard request contexts.
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
//	    max_length: 128
//
// Handlers can read the validated ID from Gin and pass the request context to
// downstream work. FromRequest and FromContext provide the same value to code
// that does not depend on Gin:
//
//	func submit(c *gin.Context) {
//		id, ok := requestid.FromGin(c)
//		if !ok {
//			c.AbortWithStatus(http.StatusInternalServerError)
//			return
//		}
//		log.Printf("request %s", id)
//		doWork(c.Request.Context())
//		c.Status(http.StatusAccepted)
//	}
//
// Missing, duplicate, malformed, or oversized inbound IDs are replaced. The
// middleware runs before accesslog so access records can safely consume the
// validated response header. Bundle and ordinary imports are side-effect free.
// Applications that prefer xbc.Run may opt into process-global composition
// through this package's autoload leaf.
package requestid
