// Package cors provides an XBC Web middleware plugin that validates origins
// and handles simple and preflight Cross-Origin Resource Sharing requests.
//
// # Usage
//
// Compose CORS explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		cors "github.com/xbcio/xbc/transport/web/extensions/security/cors"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			cors.Bundle(),
//		))
//	}
//
// A plugins.cors section activates the composed Definition. Prefer an explicit
// browser origin in production:
//
//	plugins:
//	  cors:
//	    allow_origins: ["https://app.example.com"]
//	    allow_methods: [GET, POST, OPTIONS]
//	    allow_headers: [Content-Type, Authorization]
//	    expose_headers: [X-Request-ID]
//	    allow_credentials: true
//	    max_age: 12h
//
// Origins are exact serialized origins; the standalone "*" wildcard is also
// supported. Wildcard origins and credentials are intentionally rejected as
// an unsafe combination. New and DefaultConfig support direct construction by
// custom hosts:
//
//	cfg := cors.DefaultConfig()
//	cfg.AllowOrigins = []string{"https://app.example.com"}
//	cfg.AllowCredentials = true
//	middleware, err := cors.New(cfg)
//	if err != nil {
//		return err
//	}
//	engine.Use(middleware.Handler())
//
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
package cors
