// Package ratelimit provides an in-process XBC Web middleware backed by
// golang.org/x/time/rate.
//
// # Usage
//
// Compose the rate limiter explicitly with the Web prelude:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		ratelimit "github.com/xbcio/xbc/transport/web/extensions/reliability/ratelimit"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ratelimit.Bundle(),
//		))
//	}
//
// A plugins.ratelimit section activates the composed Definition. Rate is the
// number of tokens replenished per second, and burst is the bucket capacity:
//
//	plugins:
//	  ratelimit:
//	    rate: 20
//	    burst: 40
//	    scope: client_ip
//
// ScopeGlobal shares one bucket within the process; ScopeClientIP maintains a
// bucket per parsed client IP and removes idle buckets. Rejected requests
// receive status 429 and Retry-After. Buckets are process-local, so replicas do
// not collectively enforce one distributed quota.
//
// Custom hosts can construct validated middleware directly:
//
//	middleware, err := ratelimit.New(ratelimit.Config{
//		Rate:  20,
//		Burst: 40,
//		Scope: ratelimit.ScopeClientIP,
//	})
//	if err != nil {
//		return err
//	}
//	engine.Use(middleware.Handler())
//
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
package ratelimit
