// Package gzip provides negotiated HTTP response compression with buffering
// for minimum-size and content-type decisions.
//
// # Usage
//
// Compose gzip explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		gzipplugin "github.com/xbcio/xbc/transport/web/extensions/response/gzip"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			gzipplugin.Bundle(),
//		))
//	}
//
// A plugins.gzip section activates the composed Definition. The following
// compresses JSON and text responses of at least 1 KiB, except export endpoints:
//
//	plugins:
//	  gzip:
//	    level: -1
//	    min_length: 1024
//	    content_types: ["application/json", "text/*"]
//	    exclude_paths: ["/exports/*"]
//
// Compression is selected only when the request accepts gzip and the response
// matches the configured size and media type. The middleware automatically
// bypasses HEAD, SSE, upgrades, range responses, Cache-Control: no-transform,
// and already encoded bodies. When timeout is present, XBC orders timeout
// outside gzip so a timed-out response cannot leak a partial encoded body.
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
package gzip
