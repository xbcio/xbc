package gin

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// recordingWriter is a minimal web.ResponseWriter for this file's tests: it
// commits synchronously on WriteHeader/Write, exactly like net/http's own
// ResponseWriter contract, so the shim's two-phase gin behaviour can be
// verified against a base writer whose Written() only flips true once a
// header has actually been sent -- never before.
type recordingWriter struct {
	recorder *httptest.ResponseRecorder
	status   int
	size     int
	written  bool
}

func newRecordingWriter(recorder *httptest.ResponseRecorder) *recordingWriter {
	return &recordingWriter{recorder: recorder}
}

func (w *recordingWriter) Header() http.Header { return w.recorder.Header() }

func (w *recordingWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.written = true
	w.status = code
	w.recorder.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.recorder.Write(b)
	w.size += n
	return n, err
}

func (w *recordingWriter) Status() int   { return w.status }
func (w *recordingWriter) Size() int     { return w.size }
func (w *recordingWriter) Written() bool { return w.written }
func (w *recordingWriter) Flush()        { w.recorder.Flush() }

var _ web.ResponseWriter = (*recordingWriter)(nil)

// TestShimForwardsTheStatusToTheWriterBeneath pins that the shim keeps no
// status of its own. The buffering middleware (gzip, timeout, idempotency)
// hold the writer they installed and read its Status when they finish, so a
// status parked in the shim would be invisible to them and the response would
// be sent with the default 200 instead. Where the writer beneath is two-phase
// -- gin's own, at the bottom of every real chain -- forwarding still records
// rather than commits; see
// TestStatusRecordedThroughAReplacedWriterSurvivesTheRestore.
func TestShimForwardsTheStatusToTheWriterBeneath(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder) // implements web.ResponseWriter
	s := newShim(base)

	s.WriteHeader(http.StatusTeapot)

	require.Equal(t, http.StatusTeapot, base.Status(), "the recorded status must land on the writer beneath")
	require.Equal(t, http.StatusTeapot, s.Status(), "the status the shim reports must come from the writer beneath")

	s.WriteHeaderNow()
	require.True(t, base.Written(), "the response must be committed once WriteHeaderNow has run")
	require.Equal(t, http.StatusTeapot, recorder.Code)
}

// TestShimWriteHeaderNowCommitsWithoutAStatusOfItsOwn pins gin's commit signal
// for the case where nothing recorded a status: the response still goes out,
// with whatever the writer beneath reports.
func TestShimWriteHeaderNowCommitsWithoutAStatusOfItsOwn(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder)
	s := newShim(base)

	s.WriteHeaderNow()

	require.True(t, base.Written(), "WriteHeaderNow must actually commit")
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestShimWriteCommitsBeforeBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder)
	s := newShim(base)

	s.WriteHeader(http.StatusCreated)
	_, err := s.Write([]byte("body"))
	require.NoError(t, err)
	require.True(t, base.Written(), "writing the body must commit the header along with it")
	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, "body", recorder.Body.String())
}

func TestShimSecondWriteHeaderAfterCommitIsIgnored(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder)
	s := newShim(base)

	s.WriteHeader(http.StatusOK)
	s.WriteHeaderNow()
	require.True(t, base.Written(), "WriteHeaderNow must actually commit -- otherwise the assertion below has no discriminating power even when it passes")
	s.WriteHeader(http.StatusInternalServerError)
	s.WriteHeaderNow()

	require.Equal(t, http.StatusOK, recorder.Code, "the status must not be overwritten once the response has been committed")
}

// bufferingWriter records a status without sending it and commits only when
// asked to, which is the shape of the wrappers gzip, timeout and idempotency
// install around a response. It is what makes the shim's own commit
// observable: a base writer that auto-commits on its first write -- net/http's
// contract, and what recordingWriter above models -- answers identically
// whether or not the shim committed first, so ordering asserted against it
// would be no assertion at all.
type bufferingWriter struct {
	header  http.Header
	pending int
	status  int
	body    []byte
	written bool
	// calls records the base-writer operations in the order they arrive, which
	// is the only place the header/body ordering is visible.
	calls []string
}

func (w *bufferingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *bufferingWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.written = true
	w.status = code
	w.calls = append(w.calls, "commit")
}

func (w *bufferingWriter) Write(b []byte) (int, error) {
	w.calls = append(w.calls, "body")
	w.body = append(w.body, b...)
	return len(b), nil
}

// Status answers the recorded status until the response is committed, the way
// a wrapper holding a handler's chosen status does.
func (w *bufferingWriter) Status() int {
	if w.written {
		return w.status
	}
	return w.pending
}

func (w *bufferingWriter) Size() int {
	if w.body == nil {
		return -1
	}
	return len(w.body)
}

func (w *bufferingWriter) Written() bool { return w.written }
func (w *bufferingWriter) Flush()        {}

var _ web.ResponseWriter = (*bufferingWriter)(nil)

// TestShimWriteStringReportsBytesAndCommitsBeforeTheBody pins the entry point
// gin's Context.String and Context.Redirect reach, which no other test in this
// package drives. Two independent things can be wrong here, so both are
// asserted: the byte count handed back -- gin credits it to Size, and every
// access log, metric and audit record of a response length is derived from
// that -- and the commit that must precede the body, without which the status
// the handler recorded never goes out and the body is sent under whatever the
// writer beneath defaults to.
//
// The string is deliberately multi-byte: a count taken in characters rather
// than bytes agrees with a correct one on ASCII and only diverges here.
func TestShimWriteStringReportsBytesAndCommitsBeforeTheBody(t *testing.T) {
	base := &bufferingWriter{pending: http.StatusAccepted}
	s := newShim(base)

	n, err := s.WriteString("héllo")

	require.NoError(t, err)
	require.Equal(t, 6, n, "WriteString must return the number of bytes written, not the number of characters")
	require.Equal(t, []string{"commit", "body"}, base.calls, "the body must not go out until the header has been committed")
	require.Equal(t, http.StatusAccepted, base.Status(), "the commit must carry the status recorded before it")
	require.Equal(t, "héllo", string(base.body))
	require.Equal(t, 6, base.Size(), "the number of bytes written must be credited to the writer beneath")
}

// TestGinResponseWriterSatisfiesContract is why the adapter can hand gin's own
// writer straight to a handler: gin's writer already implements every method
// the neutral contract asks for, so nothing has to be synthesized for the
// common case. A future gin upgrade that drops one of these methods must fail
// here rather than at an unrelated call site.
func TestGinResponseWriterSatisfiesContract(t *testing.T) {
	ginWriterType := reflect.TypeOf((*ginlib.ResponseWriter)(nil)).Elem()
	neutralType := reflect.TypeOf((*web.ResponseWriter)(nil)).Elem()

	require.True(t, ginWriterType.Implements(neutralType),
		"gin.ResponseWriter must satisfy web.ResponseWriter")
}
