package web

import (
	"context"
	"net/http"
)

// capRequestBody installs web.max_request_body_bytes on the request body. The
// Server runs it as the outermost handler of the startup chain, ahead of every
// middleware, so nothing can read an uncapped body: an audit, idempotency, or
// signature-verification middleware that buffers the request is bounded by it
// whatever phase it sits in.
//
// It deliberately never answers the request. Rejecting an oversized body is
// rejectOversizedRequestBody's job, and that stage is spliced after the whole
// ordered chain so the 413 still travels through request ids, access logs, CORS
// and security headers -- the same reason the unmatched-request chains in
// (*Server).Start are the global chain plus a terminal handler rather than a
// terminal handler alone.
func capRequestBody(maximum int64) Handler {
	return func(_ context.Context, c *Ctx) error {
		request := c.Request()
		if request == nil || request.Body == nil {
			return nil
		}
		request.Body = http.MaxBytesReader(c.Writer(), request.Body, maximum)
		return nil
	}
}

// rejectOversizedRequestBody answers a request that already declares more than
// web.max_request_body_bytes in its Content-Length, before any route handler
// runs. A body that declares no length is discovered only while being read,
// where the reader capRequestBody installed reports *http.MaxBytesError and
// ParamError turns it into this same 413.
func rejectOversizedRequestBody(maximum int64) Handler {
	return func(_ context.Context, c *Ctx) error {
		request := c.Request()
		if request == nil || request.ContentLength <= maximum {
			return nil
		}
		AbortProblem(c, NewProblem(http.StatusRequestEntityTooLarge, "request_body_too_large"))
		return nil
	}
}
