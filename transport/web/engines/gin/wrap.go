package gin

import (
	"context"
	"errors"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// ginMiddleware adapts a native gin.HandlerFunc into web.Middleware. Handler
// closes over the native func only to satisfy web.Handler's fixed signature
// -- the two func types can never be the same value, since Handler carries a
// context.Context and a *web.Ctx while gin.HandlerFunc carries neither -- so
// some adaptation layer is unavoidable. The closure changes nothing about
// how the wrapped middleware runs: the native context it receives is the one
// driving this request, so a native middleware calling c.Next() advances the
// same handler index it would if it had been registered with gin directly.
type ginMiddleware struct {
	handler ginlib.HandlerFunc
	order   web.Order
}

// errNotGinEngine is what Handler reports when the request is not being served
// by this adapter. It is a refusal rather than a silent skip: a gin-native
// middleware that quietly does not run is an authentication, rate limit, or
// tracing span that has disappeared without a trace.
var errNotGinEngine = errors.New("xbc: gin.Wrap middleware requires the gin engine, but this request is served by another engine")

// Wrap adapts a native gin middleware into a web.Middleware, so existing
// Gin-native middleware (otelgin, gin-contrib/*, ...) keeps working unchanged
// once xbc registers routes through the neutral Engine port.
//
// Middleware produced here is gin-specific by design: swapping engines makes
// it fail to compile, which names every gin-bound site at swap time rather
// than letting behaviour drift at runtime.
func Wrap(h ginlib.HandlerFunc, order web.Order) web.Middleware {
	if h == nil {
		panic("xbc: gin.Wrap requires a non-nil handler")
	}
	return ginMiddleware{handler: h, order: order}
}

// Handler implements web.Middleware.
//
// It also owns the seam between gin's error accumulator and xbc's error
// boundary. A gin-native middleware reports failure by pushing onto
// gin.Context.Errors instead of returning, and such middleware can only enter
// an xbc chain through Wrap, so this is where those entries are drained and
// reported as an ordinary Handler error. Reporting them from here rather than
// from the outermost handler matters: the returned error is rendered while the
// OnError scope that was active around this middleware still holds, so mappers
// contributed by an enclosing OnError still apply.
func (m ginMiddleware) Handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		gc := FromCtx(c)
		if gc == nil {
			return errNotGinEngine
		}
		before := len(gc.Errors)
		m.handler(gc)
		return reportedErrors(gc, before)
	}
}

// reportedErrors joins whatever the wrapped middleware pushed onto gin's
// accumulator during this call. Entries that predate the call belong to
// someone else, and a response that is already committed cannot be replaced,
// so neither is touched.
func reportedErrors(gc *ginlib.Context, before int) error {
	if gc.Writer == nil || gc.Writer.Written() || len(gc.Errors) <= before {
		return nil
	}
	reported := make([]error, 0, len(gc.Errors)-before)
	for _, item := range gc.Errors[before:] {
		if item != nil && item.Err != nil {
			reported = append(reported, item.Err)
		}
	}
	return errors.Join(reported...)
}

// Order implements web.Middleware.
func (m ginMiddleware) Order() web.Order { return m.order }

// FromCtx is the escape hatch back to the native context. Streaming and other
// engine-specific capabilities go through here (design §8.1).
//
// It returns nil when c is nil or when the request is served by a different
// engine, which is the only honest answer: this adapter cannot manufacture a
// *gin.Context for a request gin never saw.
func FromCtx(c *web.Ctx) *ginlib.Context {
	if c == nil {
		return nil
	}
	rc, ok := c.RequestContext().(*requestContext)
	if !ok {
		return nil
	}
	return rc.c
}

var _ web.Middleware = ginMiddleware{}
