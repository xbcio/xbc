package enginetest

import (
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"

	"github.com/xbcio/xbc/transport/web"
)

// abortIndex is far past any realistic chain length, so moving the cursor
// there ends the loop in Next without needing a separate flag.
const abortIndex = math.MaxInt8 >> 1

// errBindingUnsupported is what Bind and BindURI report. Ctx runs it through
// ParamError like any other binding failure, so a handler that binds against
// this engine still produces a safe 4xx rather than a panic or a silent zero
// value.
var errBindingUnsupported = errors.New("xbc: the neutral test engine does not implement request binding")

// requestContext implements web.RequestContext over net/http. It owns the
// handler cursor and the request's single *web.Ctx.
type requestContext struct {
	r *http.Request
	// w is the writer the response is currently written through: the recorder
	// below, or whatever wrapper a middleware installed over it.
	w web.ResponseWriter
	// rec is the request's single pending-status holder, at the bottom of the
	// writer chain. It never changes, so a wrapper installed and then removed
	// cannot take a recorded status with it.
	rec   *recorder
	chain []func(*web.Ctx)
	index int
	ctx   *web.Ctx
}

// newRequestContext binds one chain, one request, and exactly one *web.Ctx
// together. The single Ctx is not an optimization: it owns the request-scoped
// value store, so a second one would start empty and lose the matched route,
// the error resolver, and the principal.
func newRequestContext(chain []web.Handler, w http.ResponseWriter, r *http.Request) *requestContext {
	rec := newRecorder(newWriter(w))
	rc := &requestContext{
		r:     r,
		w:     rec,
		rec:   rec,
		chain: make([]func(*web.Ctx), 0, len(chain)),
		index: -1,
	}
	for _, handler := range chain {
		rc.chain = append(rc.chain, web.Handle(handler))
	}
	rc.ctx = web.NewCtx(rc)
	return rc
}

// NewRequestContext builds a web.RequestContext over an arbitrary
// writer/request pair, without a chain. Tests outside this module use it to
// obtain a *web.Ctx for a unit under test that needs one but does not need a
// server; drive Engine instead when the chain itself is the subject.
func NewRequestContext(w http.ResponseWriter, r *http.Request) web.RequestContext {
	return newRequestContext(nil, w, r)
}

// NewCtx builds a *web.Ctx over an arbitrary writer/request pair. It is the
// shorthand for the common case of NewRequestContext: a CredentialExtractor,
// ErrorMapper, or middleware test that needs a Ctx and nothing else.
func NewCtx(w http.ResponseWriter, r *http.Request) *web.Ctx {
	return newRequestContext(nil, w, r).ctx
}

func (rc *requestContext) Request() *http.Request { return rc.r }

func (rc *requestContext) SetRequest(r *http.Request) { rc.r = r }

func (rc *requestContext) Writer() web.ResponseWriter { return rc.w }

// SetWriter installs w as the writer the rest of the request renders through,
// without wrapping it.
//
// Wrapping would stack a second pending-status holder on top of the one this
// request already has, and the two do not survive a swap: a middleware that
// installs a buffering wrapper, lets the handler call Status, and then restores
// the writer it replaced would leave the recorded status behind in the holder
// that went away, and the response would be sent with the default status
// instead. One holder per request, at the bottom of the chain, is also how gin
// itself works -- a wrapper there answers Status by delegating down to the
// engine's own writer.
func (rc *requestContext) SetWriter(w web.ResponseWriter) {
	if w == nil {
		return
	}
	rc.w = w
}

// Param returns a ServeMux path value. The wildcard is spelled {name}, not an
// engine-specific form; see the package documentation.
func (rc *requestContext) Param(name string) string {
	if rc.r == nil {
		return ""
	}
	return rc.r.PathValue(name)
}

// ClientIP reports the host part of RemoteAddr. This engine has no
// trusted-proxy configuration, so it never consults a forwarding header --
// reporting one would be inventing an answer the engine has no basis for.
func (rc *requestContext) ClientIP() string {
	if rc.r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(rc.r.RemoteAddr)
	if err != nil {
		return rc.r.RemoteAddr
	}
	return host
}

// Status records the status code without committing the response. It goes
// through the writer currently installed, so a buffering wrapper sees the
// status the handler chose rather than having to guess it at commit time.
func (rc *requestContext) Status(code int) { rc.w.WriteHeader(code) }

// Bind refuses. See the package documentation for why this engine has no
// binding pipeline of its own.
func (rc *requestContext) Bind(any) error { return errBindingUnsupported }

// BindURI refuses, for the same reason as Bind.
func (rc *requestContext) BindURI(any) error { return errBindingUnsupported }

// JSON renders obj. A marshalling failure aborts the chain and is reported to
// the caller with nothing written, so the error boundary can still render a
// safe response.
func (rc *requestContext) JSON(code int, obj any) error {
	payload, err := json.Marshal(obj)
	if err != nil {
		rc.Abort()
		return err
	}
	rc.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	rc.w.WriteHeader(code)
	if !bodyAllowedForStatus(code) {
		rc.commit()
		return nil
	}
	_, writeErr := rc.w.Write(payload)
	return writeErr
}

// Next runs the remaining handlers and returns once they are done, so a
// middleware suspended in Next resumes after the chain below it has unwound.
func (rc *requestContext) Next() {
	rc.index++
	for rc.index < len(rc.chain) {
		rc.chain[rc.index](rc.ctx)
		rc.index++
	}
}

// Abort moves the cursor past every pending handler.
func (rc *requestContext) Abort() { rc.index = abortIndex }

// commit sends a status that was recorded but never written, which is how a
// handler that only calls Status still produces a response. It commits through
// the recorder rather than through the writer currently installed: a middleware
// that replaces the writer is expected to restore the one it replaced as it
// unwinds, exactly as it must under gin.
func (rc *requestContext) commit() { rc.rec.commit() }

// bodyAllowedForStatus mirrors net/http's own unexported rule: a body under
// one of these status codes corrupts the message framing.
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

// noWritten is the documented web.ResponseWriter.Size sentinel for "no write
// has happened yet".
const noWritten = -1

// writer adapts a raw http.ResponseWriter to web.ResponseWriter. It keeps
// net/http semantics exactly: WriteHeader commits.
type writer struct {
	http.ResponseWriter
	status int
	size   int
}

func newWriter(w http.ResponseWriter) *writer {
	return &writer{ResponseWriter: w, status: http.StatusOK, size: noWritten}
}

func (w *writer) Status() int   { return w.status }
func (w *writer) Size() int     { return w.size }
func (w *writer) Written() bool { return w.size != noWritten }

func (w *writer) WriteHeader(code int) {
	if code <= 0 || w.Written() {
		return
	}
	w.status = code
	w.size = 0
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(data []byte) (int, error) {
	w.WriteHeader(w.status)
	n, err := w.ResponseWriter.Write(data)
	w.size += n
	return n, err
}

func (w *writer) Flush() {
	w.WriteHeader(w.status)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the writer beneath so http.ResponseController can find
// capabilities this contract deliberately leaves out, Hijack among them.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recorder adds the two-phase status protocol RequestContext.Status requires
// on top of any web.ResponseWriter. It holds the pending status and nothing
// else: Written, Size, and the writes themselves stay with the writer beneath,
// which is the only one that knows whether a buffering wrapper has actually
// committed anything.
//
// WriteHeader records instead of committing, which is the one place this
// package deliberately departs from the net/http semantics web.ResponseWriter
// documents. gin's own writer behaves the same way -- Status records, the
// engine commits when the chain finishes -- so a middleware written against
// gin keeps its meaning here. The commit still happens for every request, from
// requestContext.commit.
type recorder struct {
	w      web.ResponseWriter
	status int
}

func newRecorder(w web.ResponseWriter) *recorder {
	return &recorder{w: w, status: http.StatusOK}
}

func (r *recorder) Header() http.Header { return r.w.Header() }

func (r *recorder) Write(data []byte) (int, error) {
	r.commit()
	return r.w.Write(data)
}

func (r *recorder) WriteHeader(code int) {
	if code > 0 && !r.w.Written() {
		r.status = code
	}
}

func (r *recorder) Flush() {
	r.commit()
	r.w.Flush()
}

func (r *recorder) Status() int {
	if r.w.Written() {
		return r.w.Status()
	}
	return r.status
}

func (r *recorder) Size() int     { return r.w.Size() }
func (r *recorder) Written() bool { return r.w.Written() }

// Unwrap exposes the wrapped writer for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.w }

func (r *recorder) commit() {
	if !r.w.Written() {
		r.w.WriteHeader(r.status)
	}
}

var (
	_ web.RequestContext = (*requestContext)(nil)
	_ web.ResponseWriter = (*writer)(nil)
	_ web.ResponseWriter = (*recorder)(nil)
)
