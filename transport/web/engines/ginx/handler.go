package ginx

import (
	"context"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler adapts a native gin terminal handler into a web.Handler, so one route
// whose body is still gin-shaped can stay in the xbc route table and keep xbc's
// route metadata, authentication policy, and error boundary around it.
//
// Handler is the terminal-handler counterpart of Wrap: Wrap adapts gin
// middleware, which runs around a chain and advances it with c.Next, while
// Handler adapts the handler that ends the chain. A route registered through
// the Engine port hands this adapter the native context gin itself is routing
// the request through -- fetched with FromCtx -- so the path parameters, the
// request, and the response writer are the ones gin's matcher and writer
// already produced, not a reconstruction. That is what lets a handler keep
// reading c.Param, binding with c.ShouldBindJSON, and responding with c.JSON
// exactly as it did under a gin engine.
//
// It also owns the seam between gin's error accumulator and xbc's error
// boundary, the same seam Wrap implements: a gin-native handler reports failure
// by pushing onto gin.Context.Errors instead of returning, so whatever it
// appends during its own call is joined and returned as an ordinary web.Handler
// error. Entries that predate the call belong to someone else and are left
// alone. The returned error then travels the normal path -- web.Handle renders
// it through the active OnError mapper chain while the scope around this route
// still holds.
//
// FromCtx returns nil when the request is not being served by the gin engine,
// and Handler reports errNotGinEngine rather than panicking or silently doing
// nothing: a gin-native handler that quietly does not run is a route that has
// disappeared without a trace.
//
// Handler panics on a nil handler, matching web.Handle.
func Handler(h ginlib.HandlerFunc) web.Handler {
	if h == nil {
		panic("xbc: gin.Handler requires a non-nil handler")
	}
	return func(_ context.Context, c *web.Ctx) error {
		gc := FromCtx(c)
		if gc == nil {
			return errNotGinEngine
		}
		before := len(gc.Errors)
		h(gc)
		return reportedErrors(gc, before)
	}
}
