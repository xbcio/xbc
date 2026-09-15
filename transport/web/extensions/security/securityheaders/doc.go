// Package securityheaders applies configurable browser security response
// headers to normal, rejected, and preflight Web responses.
//
// # Usage
//
// Compose security headers explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		securityheaders "github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			securityheaders.Bundle(),
//		))
//	}
//
// A plugins.securityheaders section activates the composed Definition. Safe
// defaults are supplied for CSP, frame, referrer, permissions, opener,
// MIME-sniffing, XSS, and HSTS policies; individual string-valued policies can
// be disabled with an empty value:
//
//	plugins:
//	  securityheaders:
//	    content_security_policy: "default-src 'self'; object-src 'none'"
//	    frame_options: DENY
//	    referrer_policy: no-referrer
//	    hsts_enabled: true
//	    hsts_max_age: 31536000
//	    hsts_include_subdomains: true
//	    hsts_only_https: true
//	    hsts_trust_forwarded_proto: false
//
// HSTS is emitted only for TLS requests by default. Enable forwarded-proto
// trust only behind a trusted proxy; otherwise a client could spoof HTTPS.
// Preload configuration is validated against the required max-age and
// includeSubDomains settings. Bundle and ordinary imports are side-effect
// free. Applications that prefer xbc.Run may opt into process-global
// composition through this package's autoload leaf.
package securityheaders
