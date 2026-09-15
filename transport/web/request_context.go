package web

import "net/http"

// RequestContext is the per-request surface an engine adapter implements.
// Everything else on Ctx is derived by xbc from these twelve methods, so an
// adapter stays small and no engine-specific concept leaks upward.
type RequestContext interface {
	Request() *http.Request
	SetRequest(r *http.Request)
	Writer() ResponseWriter
	SetWriter(w ResponseWriter)
	Param(name string) string

	// ClientIP is forwarded rather than derived: the correct answer depends on
	// how the engine parsed Options.TrustedProxies, and that state belongs to
	// the engine. Deriving it here would fork from the engine's own answer.
	ClientIP() string

	// Status records the status code without committing the response. gin's
	// Status is two-phase; calling WriteHeader directly here would flip
	// Written() immediately and trip nine "do not write twice" guards.
	Status(code int)

	Bind(obj any) error
	BindURI(obj any) error
	JSON(code int, obj any) error

	Next()
	Abort()
}
