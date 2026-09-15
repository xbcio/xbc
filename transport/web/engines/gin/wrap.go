package gin

import (
	"context"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// ginMiddleware adapts a native gin.HandlerFunc into web.Middleware. Handler
// closes over the native func only to satisfy web.Handler's fixed signature
// -- the two func types can never be the same value, since Handler carries a
// context.Context and a *web.Ctx while gin.HandlerFunc carries neither -- so
// some adaptation layer is unavoidable. The closure changes nothing about
// how the wrapped middleware runs: c.Gin().Next() still advances the same
// underlying *gin.Context's own handler index, so a native middleware calling
// c.Next() from inside this wrapper behaves exactly as it would registered
// directly with gin.
type ginMiddleware struct {
	handler ginlib.HandlerFunc
	order   web.Order
}

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
func (m ginMiddleware) Handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		m.handler(c.Gin())
		return nil
	}
}

// Order implements web.Middleware.
func (m ginMiddleware) Order() web.Order { return m.order }

// FromCtx is the escape hatch back to the native context. Streaming and other
// engine-specific capabilities go through here (design §8.1).
func FromCtx(c *web.Ctx) *ginlib.Context {
	if c == nil {
		return nil
	}
	return c.Gin()
}

var _ web.Middleware = ginMiddleware{}
