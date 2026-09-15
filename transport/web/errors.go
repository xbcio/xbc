package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corelog "github.com/xbcio/xbc/log"
)

const errorResolverContextKey = "xbc/web.errorResolver"

// Handler is an application handler that reports failure through Go's normal
// error return path. It receives the request's own cancellable context and
// its Ctx. Handle adapts it to an engine adapter's per-handler calling
// convention and sends every returned error through the request's OnError
// mapper chain.
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

// OnError returns an error boundary. It makes mappers available to AbortError
// while the inner chain is running. Nested OnError middleware composes its
// mappers in outer-to-inner order, so application plugins can add focused
// converters without replacing Web's safe built-in fallbacks.
//
// OnError handles ordinary errors only. Panics remain the responsibility of a
// recovery middleware, which can safely account for partially written
// responses and broken connections.
//
// Errors an engine-native middleware reports through the engine's own error
// accumulator instead of Handler's return path are drained by the adapter that
// wraps that middleware, which reports them as an ordinary Handler error. They
// therefore reach this boundary inside its scope, mapped by the mappers
// registered here, without OnError knowing that any such accumulator exists.
func OnError(mappers ...ErrorMapper) Handler {
	scopedMappers := append([]ErrorMapper(nil), mappers...)
	return func(_ context.Context, c *Ctx) error {
		if c == nil {
			return nil
		}
		parent := resolverFor(c)
		resolver := parent.withMappers(scopedMappers)
		c.Set(errorResolverContextKey, resolver)
		defer c.Set(errorResolverContextKey, parent)
		c.Next()
		return nil
	}
}

// Handle adapts an error-returning Handler to the plain per-handler function an
// engine adapter drives its chain with. ctx is the request's own context
// (Ctx.Request().Context()), not context.Background(), so cancellation and
// deadlines set by upstream middleware (timeouts, client disconnects) propagate
// into the handler. Errors abort the remaining handler chain and are rendered
// immediately. Immediate rendering is important: buffering middleware such as
// timeout, gzip, and idempotency must observe the final response while their
// own deferred work unwinds.
//
// The adapter passes the request's single shared Ctx in; it must not build a
// fresh one per handler. See NewCtx for what a second Ctx would lose.
func Handle(handler Handler) func(*Ctx) {
	if handler == nil {
		panic("xbc: web.Handle requires a non-nil handler")
	}
	return func(c *Ctx) {
		request := c.Request()
		ctx := context.Background()
		if request != nil {
			ctx = request.Context()
		}
		if err := handler(ctx, c); err != nil {
			AbortError(c, err)
		}
	}
}

// AbortError stops the remaining handler chain and renders err with the
// active OnError mapper chain. It is the middleware-oriented counterpart of
// returning an error from Handler. Immediate rendering ensures buffering
// middleware observes the final error response while unwinding. When no
// OnError middleware is installed, the safe built-in mappings still apply.
func AbortError(c *Ctx, err error) {
	if c == nil || err == nil {
		return
	}
	c.Abort()
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
//
// It stays engine-neutral on purpose: Server installs it as the second handler
// of the startup chain, where no engine type is in scope.
func (r *errorResolver) attach(c *Ctx) {
	c.Set(errorResolverContextKey, r)
}

func resolverFor(c *Ctx) *errorResolver {
	if c != nil {
		if value, ok := c.Get(errorResolverContextKey); ok {
			if resolver, valid := value.(*errorResolver); valid && resolver != nil {
				return resolver
			}
		}
	}
	return fallbackErrorResolver
}

func (r *errorResolver) write(c *Ctx, err error) {
	if c == nil || err == nil {
		return
	}
	if c.Writer() == nil || c.Writer().Written() {
		r.log(err, 0, c, true)
		return
	}

	problem := r.mapError(c, err)
	if problem.Status >= http.StatusInternalServerError {
		r.log(err, problem.Status, c, false)
	}
	AbortProblem(c, problem)
}

// mapError is called only from write, which is reached exclusively from
// framework-internal transport plumbing (AbortError and resolveErrors), never
// from application code. It hands c straight to every configured ErrorMapper
// -- the application-facing contract -- so each one sees the same fixed
// surface a Handle-registered handler does.
func (r *errorResolver) mapError(c *Ctx, err error) ProblemDetail {
	for _, mapper := range r.mappers {
		if mapper == nil {
			continue
		}
		if problem, ok := mapper.MapError(c, err); ok {
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

func (r *errorResolver) log(err error, status int, c *Ctx, responseWritten bool) {
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
	if c != nil && c.Request() != nil {
		fields = append(fields, "method", c.Request().Method)
		if c.Request().URL != nil {
			fields = append(fields, "path", c.Request().URL.Path)
		}
	}
	if c != nil {
		if route, ok := CurrentRoute(c); ok {
			fields = append(fields, "route", route.Path, "route_name", route.Name)
		}
	}
	logger.Error("http request failed", fields...)
}
