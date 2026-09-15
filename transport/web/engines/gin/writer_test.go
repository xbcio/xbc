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

	require.Equal(t, http.StatusTeapot, base.Status(), "记录的状态码必须落到下层写入器上")
	require.Equal(t, http.StatusTeapot, s.Status(), "shim 报告的状态码应来自下层写入器")

	s.WriteHeaderNow()
	require.True(t, base.Written(), "WriteHeaderNow 之后响应必须已提交")
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

	require.True(t, base.Written(), "WriteHeaderNow 必须真正提交")
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestShimWriteCommitsBeforeBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder)
	s := newShim(base)

	s.WriteHeader(http.StatusCreated)
	_, err := s.Write([]byte("body"))
	require.NoError(t, err)
	require.True(t, base.Written(), "写 body 必须顺带提交 header")
	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, "body", recorder.Body.String())
}

func TestShimSecondWriteHeaderAfterCommitIsIgnored(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder)
	s := newShim(base)

	s.WriteHeader(http.StatusOK)
	s.WriteHeaderNow()
	require.True(t, base.Written(), "WriteHeaderNow 必须真正提交，否则以下断言即使全绿也没有区分力")
	s.WriteHeader(http.StatusInternalServerError)
	s.WriteHeaderNow()

	require.Equal(t, http.StatusOK, recorder.Code, "提交后的状态码不得被覆盖")
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
		"gin.ResponseWriter 必须满足 web.ResponseWriter")
}
