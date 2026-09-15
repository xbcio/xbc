// Package timeout applies cooperative per-request deadlines and buffers handler
// output so a deadline can return one clean 504 without concurrent writes or a
// partially committed response.
//
// # Usage
//
// Compose timeout explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		timeout "github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			timeout.Bundle(),
//		))
//	}
//
// A plugins.timeout section activates the composed Definition. Excluded paths
// are exact or trailing-'*' prefixes, while excluded route names match
// Route.Name exactly:
//
//	plugins:
//	  timeout:
//	    duration: 5s
//	    exclude_paths: ["/events", "/downloads/*"]
//	    exclude_routes: ["reports.stream"]
//
// Deadline handling is cooperative: handlers and the work they call must
// observe c.Request.Context(). For example:
//
//	func wait(c *gin.Context) {
//		select {
//		case <-time.After(time.Second):
//			c.Status(http.StatusNoContent)
//		case <-c.Request.Context().Done():
//			return // timeout writes the final 504 response
//		}
//	}
//
// SSE and upgrade requests are bypassed automatically. A handler that starts
// streaming cannot have its already committed response replaced. When gzip is
// present, timeout runs outside it so only a complete response is compressed
// and committed. Bundle and ordinary imports are side-effect free. Applications
// that prefer xbc.Run may opt into process-global composition through this
// package's autoload leaf.
package timeout
