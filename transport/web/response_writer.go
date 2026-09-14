package web

import "net/http"

// ResponseWriter is the response-writing contract Web middleware intercepts.
// It is deliberately narrower than gin.ResponseWriter: it carries only what
// the framework's own guards read, plus the net/http vocabulary a buffering
// wrapper must be able to forward.
//
// Status, Size, and Written live on the writer rather than on Ctx because the
// guards that call them -- the "do not write this response twice" checks in
// problem.go and errors.go -- must observe the outermost wrapper. A recorder
// installed beneath a buffering wrapper would answer Written() == false while
// the handler's bytes still sit in that wrapper's buffer, the guard would not
// fire, and the response body would be written twice.
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

	// Size returns the number of body bytes already written.
	Size() int

	// Written reports whether the response has been committed.
	Written() bool
}
