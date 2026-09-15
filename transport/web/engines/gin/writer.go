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
// The neutral face uses net/http semantics: WriteHeader commits. gin's face
// is two-phase: Status only records, WriteHeaderNow commits. That two-phase
// protocol lives in gin's own responseWriter, not in its Context, so it is
// replicated here rather than leaking into the neutral interface.
type shim struct {
	w      web.ResponseWriter
	status int
}

func newShim(w web.ResponseWriter) *shim {
	return &shim{w: w, status: http.StatusOK}
}

func (s *shim) WriteHeader(code int) {
	if code > 0 && !s.w.Written() {
		s.status = code
	}
}

func (s *shim) WriteHeaderNow() {
	if !s.w.Written() {
		s.w.WriteHeader(s.status)
	}
}

func (s *shim) Write(b []byte) (int, error) {
	s.WriteHeaderNow()
	return s.w.Write(b)
}

func (s *shim) WriteString(value string) (int, error) {
	return s.Write([]byte(value))
}

func (s *shim) Header() http.Header { return s.w.Header() }
func (s *shim) Status() int         { return s.status }
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
