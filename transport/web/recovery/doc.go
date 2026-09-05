// Package recovery provides the outermost supported Web middleware boundary
// for recovering panics and returning a clean JSON 500 response when no
// response has yet been committed.
//
// # Usage
//
// Compose recovery explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		recovery "github.com/xbcio/xbc/transport/web/recovery"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			recovery.Bundle(),
//		))
//	}
//
// A plugins.recovery section activates the composed Definition. Stack controls
// whether the server-side recovery log includes runtime stack data:
//
//	plugins:
//	  recovery:
//	    stack: true
//
// Recovery runs in web.PhaseRecover, outside normal observation, centralized
// error mapping, security, authentication, and business middleware. It never
// logs panic values, request headers, query strings, or bodies. If a handler
// already committed a response, recovery cannot replace it; broken-connection
// panics are recorded and aborted without attempting another write. Bundle and
// ordinary imports are side-effect free. Applications that prefer xbc.Run may
// opt into process-global composition through this package's autoload leaf.
package recovery
