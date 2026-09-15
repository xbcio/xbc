package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// textContentType is the media type Ctx.String renders with, matching what
// every engine's own plain-text renderer announces.
const textContentType = "text/plain; charset=utf-8"

// Ctx exposes request reading and response writing for handlers registered
// through Router. Its method set is deliberately fixed and small: reading the
// request, writing the response, request-scoped values, chain control, and a
// single named escape hatch back to the engine port. In particular Ctx has no
// Deadline, Done, or Err methods, so it can never satisfy context.Context --
// see TestCtxDoesNotImplementContextContext in context_test.go for the guard
// that keeps that true.
//
// Ctx holds a RequestContext, never a concrete engine context. Twelve of its
// methods forward straight to that port; the rest are derived here, once, so
// every engine adapter stays small and no engine-specific concept leaks
// upward.
type Ctx struct {
	rc RequestContext
	// values is Ctx's own request-scoped store. See Set for why it is not the
	// engine's.
	values map[string]any
	// queryCache holds the parsed query string, and queryCacheFor is the exact
	// RawQuery it was parsed from. See query for why the two travel together.
	queryCache    url.Values
	queryCacheFor string
}

// newCtx wraps an engine adapter's per-request surface. Framework-internal
// construction goes through it; NewCtx is the exported door for code outside
// this package.
func newCtx(rc RequestContext) *Ctx {
	return &Ctx{rc: rc}
}

// NewCtx builds a Ctx over an engine adapter's per-request surface. Adapters
// and the neutral test engine construct one per request; nothing else should.
//
// An adapter must build exactly one Ctx per request and hand that same value
// to every handler in the chain. Ctx now owns the request-scoped value store
// (see Set), so a second Ctx over the same request would silently start from
// an empty map and lose everything upstream middleware published -- including
// the matched route and the authenticated principal.
//
// Two concrete needs keep it exported, both observed rather than anticipated.
// A plugin implementing CredentialExtractor or ErrorMapper lives in its own
// module and cannot reach newCtx, so without this every such plugin would
// reimplement the same trick -- drive a synthetic request through a real
// engine and capture the Ctx from inside a no-op handler. Three plugins
// (apikey, jwt, session) needed it on the first day of the migration. That
// trick also cannot produce a Ctx whose request is nil, which left the
// nil-request guards in those extractors permanently untestable.
//
// NewCtx does not validate rc. A Ctx built around a RequestContext with no
// request reports a nil Request(), which is exactly the state those guards
// exist to absorb.
func NewCtx(rc RequestContext) *Ctx {
	return newCtx(rc)
}

// -- Reading the request --

// Bind runs the engine's automatic content-type binding against obj. A failure
// is adapted through ParamError so it flows into Web's centralized Problem
// Detail mapping instead of writing a response itself; see ParamError's
// documentation for the safe error shapes it produces.
//
// The wrapping belongs here rather than in an adapter on purpose: turning a
// binding failure into a safe 400 is a decision xbc makes once for every
// engine, and RequestContext.Bind deliberately returns the engine's own raw
// error. Forwarding that error unwrapped would leave a binding failure with no
// mapping at all, so it would surface as a 500 carrying the engine's own
// message.
func (c *Ctx) Bind(obj any) error {
	if err := c.rc.Bind(obj); err != nil {
		return ParamError(err, obj)
	}
	return nil
}

// BindURI runs the engine's URI parameter binding against obj, adapting
// failures the same way Bind does.
func (c *Ctx) BindURI(obj any) error {
	if err := c.rc.BindURI(obj); err != nil {
		return ParamError(err, obj)
	}
	return nil
}

// Param returns a URL path parameter, or "" if it is not present.
func (c *Ctx) Param(key string) string {
	return c.rc.Param(key)
}

// Query returns a URL query parameter, or "" if it is not present.
//
// The parsed query string is cached on the Ctx, keyed by the RawQuery it came
// from. See query for why that key is what makes the cache safe.
func (c *Ctx) Query(key string) string {
	value, _ := c.query(key)
	return value
}

// DefaultQuery returns a URL query parameter, or defaultValue if it is not
// present. A parameter present with an empty value is present.
func (c *Ctx) DefaultQuery(key, defaultValue string) string {
	if value, found := c.query(key); found {
		return value
	}
	return defaultValue
}

// query parses the request's query string at most once per distinct RawQuery.
//
// url.URL.Query allocates a fresh url.Values on every call, so a handler or a
// middleware reading several parameters -- or one parameter in a loop -- paid
// a full re-parse each time. Measured on the benchmark in engines/gin, one
// lookup cost 413ns and three allocations without this cache, and 14ns with no
// allocation at all once it is in place.
//
// The cache is keyed by the RawQuery it was parsed from rather than simply
// built once, because SetContext replaces the *http.Request. It preserves the
// URL today, so the key always matches and the cache always hits; keying on it
// anyway means a future path that does swap the URL gets a re-parse instead of
// a stale answer. Staleness is made unrepresentable rather than merely avoided.
func (c *Ctx) query(key string) (string, bool) {
	request := c.rc.Request()
	if request == nil || request.URL == nil {
		return "", false
	}
	if c.queryCache == nil || c.queryCacheFor != request.URL.RawQuery {
		c.queryCache = request.URL.Query()
		c.queryCacheFor = request.URL.RawQuery
	}
	if values, found := c.queryCache[key]; found && len(values) > 0 {
		return values[0], true
	}
	return "", false
}

// GetHeader returns a request header value, or "" if it is not present.
func (c *Ctx) GetHeader(key string) string {
	request := c.rc.Request()
	if request == nil {
		return ""
	}
	return request.Header.Get(key)
}

// ClientIP reports the engine's own answer for the request's client address.
// It is forwarded rather than derived: the correct answer depends on how the
// engine parsed its trusted-proxy configuration, and that state belongs to the
// engine. Deriving it from RemoteAddr and headers here would fork from the
// answer the engine itself gives.
func (c *Ctx) ClientIP() string { return c.rc.ClientIP() }

// -- Writing the response --

// Status records the HTTP response status code without committing the
// response. See RequestContext.Status for why recording and committing are
// separate.
func (c *Ctx) Status(code int) {
	c.rc.Status(code)
}

// SetHeader sets a response header, or deletes it when value is empty.
// Deleting on an empty value is the established behaviour of every call site
// that passes a possibly-absent value through.
func (c *Ctx) SetHeader(key, value string) {
	writer := c.rc.Writer()
	if writer == nil {
		return
	}
	if value == "" {
		writer.Header().Del(key)
		return
	}
	writer.Header().Set(key, value)
}

// JSON writes obj as a JSON response with the given status code. A render
// failure aborts the chain and is rendered through the active OnError mapper
// chain, so a response that could not be marshalled becomes a safe Problem
// Detail instead of a silently truncated body.
func (c *Ctx) JSON(code int, obj any) {
	if err := c.rc.JSON(code, obj); err != nil {
		AbortError(c, err)
	}
}

// String writes a formatted plain-text response with the given status code.
func (c *Ctx) String(code int, format string, values ...any) {
	writer := c.rc.Writer()
	if writer == nil {
		return
	}
	c.rc.Status(code)
	if len(writer.Header().Values("Content-Type")) == 0 {
		writer.Header().Set("Content-Type", textContentType)
	}
	if !bodyAllowedForStatus(code) {
		return
	}
	if len(values) > 0 {
		_, _ = fmt.Fprintf(writer, format, values...)
		return
	}
	_, _ = io.WriteString(writer, format)
}

// Data writes raw bytes with an explicit content type and status code.
func (c *Ctx) Data(code int, contentType string, data []byte) {
	writer := c.rc.Writer()
	if writer == nil {
		return
	}
	c.rc.Status(code)
	if contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	if !bodyAllowedForStatus(code) {
		return
	}
	_, _ = writer.Write(data)
}

// bodyAllowedForStatus mirrors net/http's own unexported rule. A body written
// under one of these status codes corrupts the message framing, so String and
// Data record the status and stop rather than write one. The engine commits
// the recorded status when the chain finishes.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent:
		return false
	case status == http.StatusNotModified:
		return false
	}
	return true
}

// -- Request-scoped values --

// Set stores a request-scoped value, visible to every handler that shares this
// Ctx.
//
// The store belongs to this Ctx, not to the engine. A third-party engine-native
// middleware adapted into the chain keeps writing into the engine's own
// per-request store, and the two are not connected in either direction: a value
// that middleware writes with the engine's setter is not visible to Get, and a
// value written here is not visible to the engine's getter. Code that needs to
// bridge the two must copy explicitly, through the engine escape hatch.
//
// A single store is the point rather than an omission. Two stores for one
// request is exactly the shape that makes a value appear to have been published
// while every reader on the other side sees nothing.
func (c *Ctx) Set(key string, value any) {
	if c.values == nil {
		c.values = make(map[string]any)
	}
	c.values[key] = value
}

// Get retrieves a request-scoped value previously stored with Set. See Set for
// why it does not read the engine's own per-request store.
func (c *Ctx) Get(key string) (any, bool) {
	value, found := c.values[key]
	return value, found
}

// Principal returns the verified identity published by an upstream
// authentication plugin, if any. It reuses CurrentPrincipal so Ctx does not
// duplicate principal storage or copy semantics.
func (c *Ctx) Principal() (Principal, bool) {
	return CurrentPrincipal(c)
}

// SetContext publishes ctx to every downstream reader by rewriting the
// request. Ctx deliberately does not hold a context.Context of its own: the
// single source of truth must stay the request, because Handler's first
// parameter is read off c.Request().Context() at call time, and third-party
// engine-native middleware reads the request's context too. A Ctx-owned
// context would fork from the request's and leave both sets of readers
// silently on a stale value.
func (c *Ctx) SetContext(ctx context.Context) {
	request := c.rc.Request()
	if request == nil {
		return
	}
	c.rc.SetRequest(request.WithContext(ctx))
}

// -- Chain control --

// Next suspends the calling middleware until the rest of the chain has run,
// then resumes it. It is deliberately kept instead of a wrapping
// Wrap(next) Handler model: a third-party engine-native middleware calls the
// engine's own Next, and only a chain the engine itself advances can splice
// xbc middleware and engine-ecosystem middleware into one ordered list.
func (c *Ctx) Next() {
	c.rc.Next()
}

// Abort prevents any pending handlers in the chain from running. Abort does
// not render a response by itself; a handler that aborts is responsible for
// having already written one (for example via JSON, String, or Data), or
// should return an error from Handler instead so the error boundary renders
// it.
func (c *Ctx) Abort() {
	c.rc.Abort()
}

// -- Escape hatches --

// Request returns the underlying *http.Request.
func (c *Ctx) Request() *http.Request {
	return c.rc.Request()
}

// Writer returns the response writer, for streaming, SSE, or other
// response-writing needs that Ctx's fixed method set does not cover.
func (c *Ctx) Writer() ResponseWriter {
	return c.rc.Writer()
}

// SetWriter installs w as the writer the rest of the request renders through.
// Buffering middleware (gzip, timeout, idempotency) swaps a wrapper in here and
// restores the previous writer as it unwinds; the previous writer is whatever
// Writer returned before the swap.
func (c *Ctx) SetWriter(w ResponseWriter) {
	c.rc.SetWriter(w)
}

// RequestContext returns the engine adapter's per-request surface. The name is
// deliberately explicit so call sites make it obvious they are stepping outside
// Ctx's fixed contract.
//
// What it returns is the neutral port, not an engine type: an adapter that
// wants its own concrete context back type-asserts its own implementation out
// of it, which fails cleanly on any other engine. Ctx itself therefore stays
// free of engine imports, and the escape hatch stays where it belongs -- in the
// adapter that owns the engine.
func (c *Ctx) RequestContext() RequestContext {
	if c == nil {
		return nil
	}
	return c.rc
}
