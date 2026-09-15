package gin

import (
	"bufio"
	"net"
	"net/http"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// shim adapts a neutral web.ResponseWriter back into gin.ResponseWriter.
//
// The neutral face uses net/http semantics: WriteHeader commits. gin's face is
// two-phase: Status only records, WriteHeaderNow commits. That two-phase
// protocol lives in gin's own responseWriter, not in its Context, and this shim
// bridges the two without adding a status of its own.
//
// Holding a status here is what the previous shape got wrong. A request has
// exactly one pending-status holder -- gin's own writer, at the bottom of the
// chain -- and the buffering middleware (gzip, timeout, idempotency) all
// install a wrapper, let the handler run, and then restore the writer they
// replaced. A status recorded into a holder that lives only as long as the
// wrapper is lost at that restore, and the response goes out with the default
// 200 rather than the status the handler chose.
type shim struct {
	w web.ResponseWriter
}

func newShim(w web.ResponseWriter) *shim {
	return &shim{w: w}
}

// WriteHeader forwards, so the record lands wherever the holder actually is:
// in a buffering wrapper that keeps its own status, or in gin's writer at the
// bottom of the chain.
func (s *shim) WriteHeader(code int) { s.w.WriteHeader(code) }

// WriteHeaderNow is gin's commit signal. It asks the neutral writer currently
// installed to send its status rather than reaching past it to gin's own
// writer: a buffering wrapper must stay free to hold the response back, and
// gin commits its own writer once the chain has unwound in any case.
func (s *shim) WriteHeaderNow() {
	if s.w.Written() {
		return
	}
	status := s.w.Status()
	if status <= 0 {
		status = http.StatusOK
	}
	s.w.WriteHeader(status)
}

func (s *shim) Write(b []byte) (int, error) {
	s.WriteHeaderNow()
	return s.w.Write(b)
}

func (s *shim) WriteString(value string) (int, error) {
	return s.Write([]byte(value))
}

func (s *shim) Header() http.Header { return s.w.Header() }
func (s *shim) Status() int         { return s.w.Status() }
func (s *shim) Size() int           { return s.w.Size() }
func (s *shim) Written() bool       { return s.w.Written() }
func (s *shim) Flush()              { s.w.Flush() }

// Hijack routes through http.ResponseController, the standard-library path
// that follows Unwrap() down to the real writer.
func (s *shim) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(s.w).Hijack()
}

// CloseNotify and Pusher exist only to satisfy gin.ResponseWriter. The former
// is deprecated in net/http; the latter is HTTP/2 push, which no xbc code uses.
func (s *shim) CloseNotify() <-chan bool { return make(chan bool) }
func (s *shim) Pusher() http.Pusher      { return nil }

func (s *shim) Unwrap() http.ResponseWriter { return s.w }

var _ ginlib.ResponseWriter = (*shim)(nil)
