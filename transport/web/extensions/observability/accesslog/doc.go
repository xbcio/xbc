// Package accesslog emits one structured, secret-minimized record for each
// HTTP request, including requests that unwind through panic recovery. Query
// strings, headers, cookies, bodies, errors, and recovered panic values are
// deliberately excluded from records.
//
// # Usage
//
// Compose access logging explicitly with the Web prelude. Request-ID middleware
// is optional, but including its Bundle lets accesslog record the validated
// response request ID:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		accesslog "github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
//		requestid "github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			accesslog.Bundle(),
//			requestid.Bundle(),
//		))
//	}
//
// A plugins.accesslog section activates and configures the composed Definition.
// For example:
//
//	plugins:
//	  requestid:
//	    header: X-Request-ID
//	    trust_incoming: false
//	    max_length: 128
//	  accesslog:
//	    request_id_header: X-Request-ID
//	    skip_paths: ["/healthz", "/assets/*"]
//	    slow_request: 500ms
//	    trust_proxy_headers: false
//
// A skip path is either exact or a prefix ending in '*'. Proxy headers should
// be trusted only when every request passes through a trusted reverse proxy.
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
package accesslog
