package web

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Ctx exposes request reading and response writing for handlers registered
// through HandleCtx. Its method set is deliberately fixed and small: reading
// the request, writing the response, request-scoped values, chain control,
// and a small set of named escape hatches. In particular Ctx has no
// Deadline, Done, or Err methods, so it can never satisfy context.Context --
// see TestCtxDoesNotImplementContextContext in context_test.go for the guard
// that keeps that true.
type Ctx struct {
	c *gin.Context
}

// newCtx wraps a live *gin.Context. Framework-internal construction goes
// through it; NewCtx is the exported door for code outside this package.
func newCtx(c *gin.Context) *Ctx {
	return &Ctx{c: c}
}

// NewCtx wraps a live *gin.Context so code outside this package can build the
// Ctx a handler would receive. It is the exact inverse of Ctx.Gin, and adds no
// gin exposure that Gin does not already add: gin is in the exported surface
// either way, once in the return position and once in the parameter position.
//
// Two concrete needs justify it, both observed rather than anticipated. A
// plugin implementing CredentialExtractor or ErrorMapper lives in its own
// module and cannot reach newCtx, so without this every such plugin would
// reimplement the same trick -- drive a synthetic gin.Context through Handle
// and capture the Ctx from inside a no-op handler. Three plugins (apikey, jwt,
// session) needed it on the first day of the migration. That trick also cannot
// produce a Ctx whose request is nil, which left the nil-request guards in
// those extractors permanently untestable.
//
// NewCtx does not validate c. A Ctx built around a gin.Context with no request
// will report a nil Request(), which is exactly the state those guards exist
// to absorb.
func NewCtx(c *gin.Context) *Ctx {
	return newCtx(c)
}

// -- Reading the request --

// Bind runs Gin's automatic content-type binding against obj. A failure is
// adapted through ParamError so it flows into Web's centralized Problem
// Detail mapping instead of writing a response itself; see ParamError's
// documentation for the safe error shapes it produces.
func (c *Ctx) Bind(obj any) error {
	if err := c.c.ShouldBind(obj); err != nil {
		return ParamError(err, obj)
	}
	return nil
}

// BindURI runs Gin's URI parameter binding against obj, adapting failures
// the same way Bind does.
func (c *Ctx) BindURI(obj any) error {
	if err := c.c.ShouldBindUri(obj); err != nil {
		return ParamError(err, obj)
	}
	return nil
}

// Param returns a URL path parameter, or "" if it is not present.
func (c *Ctx) Param(key string) string {
	return c.c.Param(key)
}

// Query returns a URL query parameter, or "" if it is not present.
func (c *Ctx) Query(key string) string {
	return c.c.Query(key)
}

// DefaultQuery returns a URL query parameter, or defaultValue if it is not
// present.
func (c *Ctx) DefaultQuery(key, defaultValue string) string {
	return c.c.DefaultQuery(key, defaultValue)
}

// GetHeader returns a request header value, or "" if it is not present.
func (c *Ctx) GetHeader(key string) string {
	return c.c.GetHeader(key)
}

// -- Writing the response --

// Status sets the HTTP response status code.
func (c *Ctx) Status(code int) {
	c.c.Status(code)
}

// SetHeader sets a response header.
func (c *Ctx) SetHeader(key, value string) {
	c.c.Header(key, value)
}

// JSON writes obj as a JSON response with the given status code.
func (c *Ctx) JSON(code int, obj any) {
	c.c.JSON(code, obj)
}

// String writes a formatted plain-text response with the given status code.
func (c *Ctx) String(code int, format string, values ...any) {
	c.c.String(code, format, values...)
}

// Data writes raw bytes with an explicit content type and status code.
func (c *Ctx) Data(code int, contentType string, data []byte) {
	c.c.Data(code, contentType, data)
}

// -- Request-scoped values --

// Set stores a request-scoped value, visible to downstream handlers and
// middleware sharing the same underlying request.
func (c *Ctx) Set(key string, value any) {
	c.c.Set(key, value)
}

// Get retrieves a request-scoped value previously stored with Set.
func (c *Ctx) Get(key string) (any, bool) {
	return c.c.Get(key)
}

// Principal returns the verified identity published by an upstream
// authentication plugin, if any. It reuses CurrentPrincipal so Ctx does not
// duplicate principal storage or copy semantics.
func (c *Ctx) Principal() (Principal, bool) {
	return CurrentPrincipal(c.c)
}

// SetContext publishes ctx to every downstream reader by rewriting the
// request. Ctx deliberately does not hold a context.Context of its own: the
// single source of truth must stay the request, because Handler's first
// parameter is read off c.Request().Context() at call time, and third-party
// gin middleware reads c.Request.Context() too. A Ctx-owned context would fork
// from the request's and leave both sets of readers silently on a stale value.
func (c *Ctx) SetContext(ctx context.Context) {
	c.c.Request = c.c.Request.WithContext(ctx)
}

// -- Chain control --

// Next suspends the calling middleware until the rest of the chain has run,
// then resumes it. It is deliberately kept instead of a wrapping
// Wrap(next) Handler model: a third-party gin.HandlerFunc calls gin's own
// c.Next(), and only a chain the engine itself advances can splice xbc
// middleware and gin-ecosystem middleware into one ordered list.
func (c *Ctx) Next() {
	c.c.Next()
}

// Abort prevents any pending handlers in the chain from running. Abort does
// not render a response by itself; a handler that aborts is responsible for
// having already written one (for example via JSON, String, or Data), or
// should return an error from Handler instead so the error boundary renders
// it.
func (c *Ctx) Abort() {
	c.c.Abort()
}

// -- Escape hatches --

// Request returns the underlying *http.Request.
func (c *Ctx) Request() *http.Request {
	return c.c.Request
}

// Writer returns the response writer, for streaming, SSE, or other
// response-writing needs that Ctx's fixed method set does not cover.
//
// The return type is the neutral ResponseWriter rather than gin's: everything
// the framework and its middleware actually read is in that contract, and
// naming the narrower type here is what keeps a gin-shaped method from
// leaking into middleware through an inferred variable.
func (c *Ctx) Writer() ResponseWriter {
	return c.c.Writer
}

// Gin returns the underlying *gin.Context. The name is deliberately explicit
// so call sites make it obvious they are stepping outside Ctx's fixed
// contract, typically to interoperate with third-party Gin middleware or
// helpers that only know about *gin.Context.
func (c *Ctx) Gin() *gin.Context {
	return c.c
}

// WrapGinHandler adapts a third-party gin.HandlerFunc -- for example otelgin
// or a gin-contrib/* middleware -- into the Handler shape Handle expects, so
// existing Gin-native middleware keeps working unchanged once a route
// registers Ctx-based handlers. The wrapped handler still receives the
// request's live *gin.Context (via Ctx.Gin), so it can call c.Next(), set
// headers, or record errors exactly as it would when registered directly
// with Gin.
//
// This function was named FromGin during design. That name was rejected:
// transport/web/extensions/observability/requestid already exports
// FromGin(c *gin.Context) (string, bool), which extracts a value out of a
// *gin.Context. WrapGinHandler instead adapts one handler function into
// another handler function -- a materially different operation -- and reusing
// "FromGin" for it would be actively misleading at call sites that import
// both packages together (a real scenario: transport/web/extensions/response
// already imports requestid.FromGin).
func WrapGinHandler(handler gin.HandlerFunc) Handler {
	if handler == nil {
		panic("xbc: web.WrapGinHandler requires a non-nil handler")
	}
	return func(_ context.Context, c *Ctx) error {
		handler(c.c)
		return nil
	}
}
