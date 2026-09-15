package gin

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestShimDefersCommitUntilWriteHeaderNow(t *testing.T) {
	recorder := httptest.NewRecorder()
	base := newRecordingWriter(recorder) // implements web.ResponseWriter
	s := newShim(base)

	s.WriteHeader(http.StatusTeapot)
	require.False(t, base.Written(), "Status 只记录，不得提交")
	require.Equal(t, http.StatusTeapot, s.Status(), "记录下来的状态码必须可读")

	s.WriteHeaderNow()
	require.True(t, base.Written(), "WriteHeaderNow 必须真正提交")
	require.Equal(t, http.StatusTeapot, recorder.Code)
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
