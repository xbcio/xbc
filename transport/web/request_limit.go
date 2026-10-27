package web

import (
	"context"
	"io"
	"net/http"
)

// requestBodyLimit is the one number a request's body is held to. It is the
// matched route's declaration (Route.MaxRequestBodyBytes) when it made one, and
// web.max_request_body_bytes when it did not, and in both cases never more than
// web.max_request_body_bytes_ceiling:
//
//	declared:   min(declaration, ceiling)
//	undeclared: min(defaultLimit, ceiling)
//
// It is the only place the three are combined, and both capRequestBody and
// rejectOversizedRequestBody call it, so the declared Content-Length check and
// the streamed-read check can never be answering different limits. That matters
// most for a body with no declared length (ContentLength == -1): it is bounded
// only by the reader capRequestBody installs, and a limit that was one value for
// the length check and another for the reader would let a chunked request
// through at the larger of them.
//
// A request with no matched route -- the framework's 404 and 405, whose chains
// are the global chain plus a terminal handler -- has no CurrentRoute and is
// held to the default rather than skipped: no route asked for more on its
// behalf, and an unmatched path is still a request a stranger made.
//
// The min against the ceiling is applied even to the default. Configuration
// validation already guarantees defaultLimit <= ceiling, so for a validated
// Server it changes nothing; it is kept so that this function alone, not the
// validator plus a convention, is what guarantees no request is ever admitted
// past the ceiling.
func requestBodyLimit(c *Ctx, defaultLimit, ceiling int64) int64 {
	limit := defaultLimit
	// The recorded value is read in place rather than through CurrentRoute,
	// which returns a defensive copy of the whole row; this runs twice on every
	// request and needs one integer from it.
	if value, ok := c.Get(currentRouteContextKey); ok {
		if route, ok := value.(RouteInfo); ok && route.MaxRequestBodyBytes > 0 {
			limit = route.MaxRequestBodyBytes
		}
	}
	return min(limit, ceiling)
}

// capRequestBody installs the request body limit on the request body. The
// Server runs it as the outermost handler of the startup chain, ahead of every
// middleware, so nothing can read an uncapped body: an audit, idempotency, or
// signature-verification middleware that buffers the request is bounded by it
// whatever phase it sits in.
//
// defaultLimit is web.max_request_body_bytes, the limit of a route that declared
// none, and ceiling is web.max_request_body_bytes_ceiling, the bound on every
// route. A route may declare any positive limit and is held to the smaller of
// that and the ceiling, so a declaration above the ceiling is cut down to it. The
// ceiling defaults to the default limit, which is what keeps a deployment that
// never set it exactly as bounded as before; see Config. The route is known here
// because Router.Handle bakes recordCurrentRoute in ahead of this stage; see
// requestBodyLimit.
//
// The cap covers every read of the body, not just the first: Request.GetBody
// -- the replay facility application middleware installs, and which an
// outbound client reads when it decides whether a request may be retried --
// is preserved and limited by the same maximum, because a replayed read that
// escaped the cap would hand out exactly the unbounded body this stage exists
// to deny.
//
// It deliberately never answers the request. Rejecting an oversized body is
// rejectOversizedRequestBody's job, and that stage is spliced after the whole
// ordered chain so the 413 still travels through request ids, access logs, CORS
// and security headers -- the same reason the unmatched-request chains in
// (*Server).Start are the global chain plus a terminal handler rather than a
// terminal handler alone.
func capRequestBody(defaultLimit, ceiling int64) Handler {
	return func(_ context.Context, c *Ctx) error {
		request := c.Request()
		if request == nil || request.Body == nil {
			return nil
		}
		maximum := requestBodyLimit(c, defaultLimit, ceiling)
		previous := request.GetBody
		request.Body = http.MaxBytesReader(c.Writer(), request.Body, maximum)
		if previous != nil {
			request.GetBody = func() (io.ReadCloser, error) {
				replay, err := previous()
				if err != nil {
					return nil, err
				}
				return http.MaxBytesReader(nil, replay, maximum), nil
			}
		}
		return nil
	}
}

// rejectOversizedRequestBody answers a request that already declares more than
// its body limit in its Content-Length, before any route handler runs. The
// limit is the one capRequestBody installed (see requestBodyLimit). A body that
// declares no length is discovered only while being read, where the reader
// capRequestBody installed reports *http.MaxBytesError and ParamError turns it
// into this same 413.
func rejectOversizedRequestBody(defaultLimit, ceiling int64) Handler {
	return func(_ context.Context, c *Ctx) error {
		request := c.Request()
		if request == nil || request.ContentLength <= requestBodyLimit(c, defaultLimit, ceiling) {
			return nil
		}
		AbortProblem(c, NewProblem(http.StatusRequestEntityTooLarge, "request_body_too_large"))
		return nil
	}
}
