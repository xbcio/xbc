package web

import "net/http"

// ResponseWriter is the response-writing contract Web middleware intercepts.
// It is deliberately narrower than gin.ResponseWriter: it carries only what
// the framework and its middleware read off a response writer, plus the
// net/http vocabulary a buffering wrapper must be able to forward.
//
// Status, Size, and Written live on the writer rather than on Ctx because
// they must be answered by the outermost wrapper. A recorder installed
// beneath a buffering wrapper would answer Written() == false while the
// handler's bytes still sit in that wrapper's buffer, so a "do not write this
// response twice" guard would not fire and the response body would be written
// twice. Only the writer the request is currently writing through knows that.
//
// All three of them have production call sites through this interface.
// Written gates the duplicate-write guards in the biz, recovery, and timeout
// middleware; biz reads it before rendering an envelope and then writes the
// payload through the embedded http.ResponseWriter. Status and Size are what
// the observability middleware (metrics, tracing, accesslog, auditlog) records
// for a finished request, so narrowing either one breaks a byte count or a
// status label that ships today.
//
// problem.go's duplicate-write guard reads Written through this interface via
// Ctx.Writer, and so does the adapter that drains an engine's own error
// accumulator before deciding whether a reported error still needs a response.
//
// Flush is part of the contract for a compile-time reason, not a stylistic
// one: a buffering wrapper's own Flush must commit its buffer and then
// forward to the writer it wraps, which it cannot do unless the wrapped type
// exposes Flush.
//
// Hijack is deliberately absent. Reach it through http.ResponseController,
// which follows the Unwrap() method a wrapper exposes. Keeping it out of this
// interface means an engine that cannot hijack does not have to pretend it
// can.
type ResponseWriter interface {
	http.ResponseWriter
	http.Flusher

	// Status returns the status code the response was, or will be, sent with.
	Status() int

	// Size returns the number of body bytes already written, or -1 when no
	// write has happened yet. The -1 sentinel is gin's documented contract:
	// callers that report a byte count normalize any negative value to 0, and
	// a wrapper that buffers must keep answering -1 until its first write so
	// "nothing written" stays distinguishable from "wrote an empty body".
	Size() int

	// Written reports whether the response has been committed.
	Written() bool
}
