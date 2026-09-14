package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	corelog "github.com/xbcio/xbc/log"
)

const errorResolverContextKey = "xbc/web.errorResolver"

// Handler is an application handler that reports failure through Go's normal
// error return path. It receives the request's own cancellable context and
// its Ctx. Handle adapts it to Gin and sends every returned error through the
// request's OnError mapper chain.
type Handler func(ctx context.Context, c *Ctx) error

// ErrorMapper turns application or infrastructure errors into safe HTTP
// Problem Details and declares its precedence independently of construction
// order. The first matching mapper wins. Mappers should use errors.Is/errors.As
// so wrapped domain errors retain their meaning.
type ErrorMapper interface {
	MapError(*Ctx, error) (ProblemDetail, bool)
	ErrorOrder() ErrorOrder
}

// ErrorMapperFunc adapts a function to an unconstrained ErrorMapper. Plugins
// that need explicit precedence should use a named type and implement both
// ErrorMapper methods.
type ErrorMapperFunc func(*Ctx, error) (ProblemDetail, bool)

// MapError implements ErrorMapper.
func (f ErrorMapperFunc) MapError(c *Ctx, err error) (ProblemDetail, bool) {
	if f == nil {
		return ProblemDetail{}, false
	}
	return f(c, err)
}

// ErrorOrder leaves function adapters unconstrained.
func (ErrorMapperFunc) ErrorOrder() ErrorOrder { return ErrorOrder{} }

// OnError returns a Gin error boundary. It makes mappers available to
// AbortError while the inner chain is running, then converts errors reported
// through gin.Context.Error when the chain unwinds. Nested OnError middleware
// composes its mappers in outer-to-inner order, so application plugins can add
// focused converters without replacing Web's safe built-in fallbacks.
//
// OnError handles ordinary errors only. Panics remain the responsibility of a
// recovery middleware, which can safely account for partially written
// responses and broken connections.
func OnError(mappers ...ErrorMapper) gin.HandlerFunc {
	scopedMappers := append([]ErrorMapper(nil), mappers...)
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		parent := resolverFor(c)
		resolver := parent.withMappers(scopedMappers)
		c.Set(errorResolverContextKey, resolver)
		defer c.Set(errorResolverContextKey, parent)
		resolver.resolveErrors(c)
	}
}

// Handle adapts an error-returning Handler to gin.HandlerFunc. ctx is the
// request's own context (gin.Context.Request.Context()), not
// context.Background(), so cancellation and deadlines set by upstream
// middleware (timeouts, client disconnects) propagate into the handler.
// Errors are recorded on gin.Context for diagnostics, abort the remaining
// handler chain, and are rendered immediately. Immediate rendering is
// important: buffering middleware such as timeout, gzip, and idempotency
// must observe the final response while their own deferred work unwinds.
func Handle(handler Handler) gin.HandlerFunc {
	if handler == nil {
		panic("xbc: web.Handle requires a non-nil handler")
	}
	return func(c *gin.Context) {
		if err := handler(c.Request.Context(), newCtx(c)); err != nil {
			AbortError(c, err)
		}
	}
}

// AbortError records err, stops the remaining Gin chain, and renders it with
// the active OnError mapper chain. It is the middleware-oriented counterpart
// of returning an error from Handler. Immediate rendering ensures buffering
// middleware observes the final error response while unwinding. When no
// OnError middleware is installed, the safe built-in mappings still apply.
func AbortError(c *gin.Context, err error) {
	if c == nil || err == nil {
		return
	}
	c.Abort()
	_ = c.Error(err)
	resolverFor(c).write(c, err)
}

type errorResolver struct {
	logger  corelog.Logger
	mappers []ErrorMapper
}

var fallbackErrorResolver = &errorResolver{logger: corelog.Nop()}

func newErrorResolver(logger corelog.Logger) *errorResolver {
	if logger == nil {
		logger = corelog.Nop()
	}
	return &errorResolver{logger: logger}
}

func (r *errorResolver) withMappers(mappers []ErrorMapper) *errorResolver {
	if r == nil {
		r = fallbackErrorResolver
	}
	combined := make([]ErrorMapper, 0, len(r.mappers)+len(mappers))
	combined = append(combined, r.mappers...)
	combined = append(combined, mappers...)
	return &errorResolver{logger: r.logger, mappers: combined}
}

// attach publishes the logger-bearing base resolver before any contributed
// middleware runs. OnError derives request-scoped mapper chains from it.
func (r *errorResolver) attach(c *gin.Context) {
	c.Set(errorResolverContextKey, r)
}

// resolveErrors catches Gin handlers or middleware that use c.Error(err).
// Error-returning handlers are resolved immediately by Handle and therefore
// reach this point with a response already written.
func (r *errorResolver) resolveErrors(c *gin.Context) {
	c.Next()
	if c.Writer == nil || c.Writer.Written() || len(c.Errors) == 0 {
		return
	}
	reported := make([]error, 0, len(c.Errors))
	for _, item := range c.Errors {
		if item != nil && item.Err != nil {
			reported = append(reported, item.Err)
		}
	}
	err := errors.Join(reported...)
	if err == nil {
		return
	}
	r.write(c, err)
}

func resolverFor(c *gin.Context) *errorResolver {
	if c != nil {
		if value, ok := c.Get(errorResolverContextKey); ok {
			if resolver, valid := value.(*errorResolver); valid && resolver != nil {
				return resolver
			}
		}
	}
	return fallbackErrorResolver
}

func (r *errorResolver) write(c *gin.Context, err error) {
	if c == nil || err == nil {
		return
	}
	if c.Writer == nil || c.Writer.Written() {
		r.log(err, 0, c, true)
		return
	}

	problem := r.mapError(c, err)
	if problem.Status >= http.StatusInternalServerError {
		r.log(err, problem.Status, c, false)
	}
	AbortProblem(c, problem)
}

// mapError still takes the live *gin.Context: it is called only from write,
// which is reached exclusively from framework-internal transport plumbing
// (AbortError and resolveErrors), never from application code. It wraps c
// into a Ctx once so every configured ErrorMapper -- the application-facing
// contract -- sees the same fixed surface a Handle-registered handler does.
func (r *errorResolver) mapError(c *gin.Context, err error) ProblemDetail {
	ctx := newCtx(c)
	for _, mapper := range r.mappers {
		if mapper == nil {
			continue
		}
		if problem, ok := mapper.MapError(ctx, err); ok {
			// A mapper claiming an error must produce an error status. In
			// particular, never reproduce the common but operationally harmful
			// convention of returning a business failure with HTTP 200.
			if problem.Status < http.StatusBadRequest || problem.Status > 599 {
				return NewProblem(http.StatusInternalServerError, "internal_server_error")
			}
			return normalizeProblem(problem, c)
		}
	}

	var requestErr *requestError
	if errors.As(err, &requestErr) && requestErr != nil {
		return requestErr.problem()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewProblem(http.StatusGatewayTimeout, "gateway_timeout")
	}
	return NewProblem(http.StatusInternalServerError, "internal_server_error")
}

func (r *errorResolver) log(err error, status int, c *gin.Context, responseWritten bool) {
	logger := r.logger
	if logger == nil {
		logger = corelog.Nop()
	}
	fields := []any{
		"status", status,
		"error", err,
		"error_type", fmt.Sprintf("%T", err),
		"response_written", responseWritten,
	}
	if c != nil && c.Request != nil {
		fields = append(fields, "method", c.Request.Method)
		if c.Request.URL != nil {
			fields = append(fields, "path", c.Request.URL.Path)
		}
	}
	if c != nil {
		if route, ok := CurrentRoute(c); ok {
			fields = append(fields, "route", route.Path, "route_name", route.Name)
		}
	}
	logger.Error("http request failed", fields...)
}
