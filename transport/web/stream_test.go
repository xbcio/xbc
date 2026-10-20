package web_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// stubRequestContext is the minimal adapter shape NewCtx explicitly allows: a
// RequestContext whose only meaningful method is Writer. BeginStream has to
// refuse a nil writer rather than dereference it, and the deadline tests below
// supply a writer of their own.
type stubRequestContext struct {
	writer web.ResponseWriter
}

func (stubRequestContext) Request() *http.Request       { return nil }
func (stubRequestContext) SetRequest(*http.Request)     {}
func (c stubRequestContext) Writer() web.ResponseWriter { return c.writer }
func (stubRequestContext) SetWriter(web.ResponseWriter) {}
func (stubRequestContext) Param(string) string          { return "" }
func (stubRequestContext) ClientIP() string             { return "" }
func (stubRequestContext) Status(int)                   {}
func (stubRequestContext) Bind(any) error               { return nil }
func (stubRequestContext) BindURI(any) error            { return nil }
func (stubRequestContext) JSON(int, any) error          { return nil }
func (stubRequestContext) Next()                        {}
func (stubRequestContext) Abort()                       {}

// deadlineResponseWriter is a neutral writer with no deadline support of its
// own; Unwrap exposes the writer beneath it, exactly as an engine adapter
// exposes the connection beneath the neutral writer. That is the path
// http.NewResponseController follows to find SetWriteDeadline.
type deadlineResponseWriter struct {
	header http.Header
	inner  http.ResponseWriter
}

func (w *deadlineResponseWriter) Header() http.Header            { return w.header }
func (w *deadlineResponseWriter) Write(data []byte) (int, error) { return len(data), nil }
func (w *deadlineResponseWriter) WriteHeader(int)                {}
func (w *deadlineResponseWriter) Flush()                         {}
func (w *deadlineResponseWriter) Status() int                    { return http.StatusOK }
func (w *deadlineResponseWriter) Size() int                      { return -1 }
func (w *deadlineResponseWriter) Written() bool                  { return false }
func (w *deadlineResponseWriter) Unwrap() http.ResponseWriter    { return w.inner }

// deadlineSetter is the http.ResponseWriter capability the controller looks
// for through Unwrap. It reports err verbatim, so a test can pin both the
// tolerated http.ErrNotSupported and a real failure.
type deadlineSetter struct {
	header http.Header
	err    error
}

func (w *deadlineSetter) Header() http.Header              { return w.header }
func (w *deadlineSetter) Write(data []byte) (int, error)   { return len(data), nil }
func (w *deadlineSetter) WriteHeader(int)                  {}
func (w *deadlineSetter) SetWriteDeadline(time.Time) error { return w.err }

func newDeadlineResponseWriter(err error) *deadlineResponseWriter {
	return &deadlineResponseWriter{
		header: make(http.Header),
		inner:  &deadlineSetter{header: make(http.Header), err: err},
	}
}

// zeroAllocationWriter is a neutral writer whose write path allocates nothing:
// Write discards its input and reports it accepted, and Flush is a no-op. A
// StreamWriter measured over it exercises the helper's own path, with no
// recorder bookkeeping or body buffer in the loop.
type zeroAllocationWriter struct {
	header http.Header
}

func (w *zeroAllocationWriter) Header() http.Header            { return w.header }
func (w *zeroAllocationWriter) Write(data []byte) (int, error) { return len(data), nil }
func (w *zeroAllocationWriter) WriteHeader(int)                {}
func (w *zeroAllocationWriter) Flush()                         {}
func (w *zeroAllocationWriter) Status() int                    { return http.StatusOK }
func (w *zeroAllocationWriter) Size() int                      { return -1 }
func (w *zeroAllocationWriter) Written() bool                  { return false }

// TestBeginStreamCommitsStatusAndContentType pins what BeginStream promises
// before a handler writes its first body byte: the status and the caller's
// content type are already on the committed response, the headers have been
// flushed, and the returned writer reaches the underlying writer.
//
// The recorder has no write-deadline support, so this test also covers the
// http.ErrNotSupported branch: a writer with no deadline to lift must not turn
// beginning a stream into a failure.
func TestBeginStreamCommitsStatusAndContentType(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/stream", nil))

	stream, err := c.BeginStream(http.StatusAccepted, "text/plain; charset=utf-8")
	require.NoError(t, err, "a fresh response must be able to begin a stream")
	require.NotNil(t, stream)

	assert.Equal(t, http.StatusAccepted, recorder.Code,
		"the status must be committed before BeginStream returns")
	assert.Equal(t, "text/plain; charset=utf-8", recorder.Result().Header.Get("Content-Type"),
		"the caller's content type must be announced in the committed headers")
	assert.True(t, recorder.Flushed,
		"BeginStream must flush so the headers reach the client before the first body byte")

	n, err := stream.Write([]byte("chunk"))
	require.NoError(t, err)
	assert.Equal(t, len("chunk"), n)
	assert.Equal(t, "chunk", recorder.Body.String(),
		"StreamWriter.Write must reach the underlying writer")
}

// TestStreamWriterWriteDoesNotAllocate pins the per-call cost contract of the
// returned writer: Write delegates and flushes, and neither copies the
// interface value into an allocation. The nil case writes no bytes so the
// recorder body does not grow; the payload case moves real bytes through a
// writer that allocates nothing of its own, because a nil write never
// exercises the payload path and a per-call copy would survive it.
func TestStreamWriterWriteDoesNotAllocate(t *testing.T) {
	t.Run("nil payload against the recorder", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/stream", nil))
		stream, err := c.BeginStream(http.StatusOK, "text/plain; charset=utf-8")
		require.NoError(t, err)

		allocs := testing.AllocsPerRun(100, func() {
			_, _ = stream.Write(nil)
		})
		assert.Zero(t, allocs, "a StreamWriter.Write call must not allocate")
	})

	t.Run("real payload against a zero-allocation writer", func(t *testing.T) {
		payload := bytes.Repeat([]byte("x"), 64)
		c := web.NewCtx(stubRequestContext{writer: &zeroAllocationWriter{header: make(http.Header)}})
		stream, err := c.BeginStream(http.StatusOK, "text/plain; charset=utf-8")
		require.NoError(t, err)

		allocs := testing.AllocsPerRun(100, func() {
			_, _ = stream.Write(payload)
		})
		assert.Zero(t, allocs, "a StreamWriter.Write call must not allocate around the payload")
	})
}

// TestBeginStreamRefusesCommittedResponse covers the double-commit guard. The
// response is committed by a handler write; BeginStream must refuse instead of
// layering a second status line, and it must not write anything of its own.
func TestBeginStreamRefusesCommittedResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/stream", nil))

	_, err := c.Writer().Write([]byte("already"))
	require.NoError(t, err)

	stream, err := c.BeginStream(http.StatusOK, "text/event-stream")
	require.Error(t, err, "a committed response cannot begin a stream")
	assert.Nil(t, stream)
	assert.True(t, strings.HasPrefix(err.Error(), "web: BeginStream "),
		"the refusal must carry the BeginStream error prefix, got %q", err.Error())
	assert.Contains(t, err.Error(), "response already written")
	assert.Equal(t, "already", recorder.Body.String(),
		"a refused BeginStream must not write anything")
}

// TestBeginStreamRefusesDegenerateCtx covers the two Ctx shapes that have no
// response to commit: a nil Ctx and a Ctx whose adapter reports no writer.
// Both must return an error rather than panic.
func TestBeginStreamRefusesDegenerateCtx(t *testing.T) {
	t.Run("nil Ctx", func(t *testing.T) {
		var c *web.Ctx
		stream, err := c.BeginStream(http.StatusOK, "text/plain")
		require.Error(t, err)
		assert.Nil(t, stream)
		assert.True(t, strings.HasPrefix(err.Error(), "web: BeginStream "),
			"the refusal must carry the BeginStream error prefix, got %q", err.Error())
	})

	t.Run("nil writer", func(t *testing.T) {
		c := web.NewCtx(stubRequestContext{})
		stream, err := c.BeginStream(http.StatusOK, "text/plain")
		require.Error(t, err)
		assert.Nil(t, stream)
		assert.True(t, strings.HasPrefix(err.Error(), "web: BeginStream "),
			"the refusal must carry the BeginStream error prefix, got %q", err.Error())
	})
}

// TestBeginStreamReportsDeadlineLiftFailure covers the untolerated deadline
// branch: when the connection's SetWriteDeadline fails with anything other
// than http.ErrNotSupported, the deadline is still in force and the caller
// must know, so BeginStream returns the wrapped cause and no writer.
func TestBeginStreamReportsDeadlineLiftFailure(t *testing.T) {
	sentinel := errors.New("deadline backend unavailable")
	c := web.NewCtx(stubRequestContext{writer: newDeadlineResponseWriter(sentinel)})

	stream, err := c.BeginStream(http.StatusOK, "text/event-stream")
	require.Error(t, err)
	assert.Nil(t, stream)
	assert.ErrorIs(t, err, sentinel,
		"the deadline failure must be wrapped so the caller can inspect it")
	assert.True(t, strings.HasPrefix(err.Error(), "web: BeginStream "),
		"the failure must carry the BeginStream error prefix, got %q", err.Error())
}

// TestBeginStreamToleratesUnsupportedWriteDeadline pins the one tolerated
// deadline outcome: http.ErrNotSupported means there is no deadline to lift
// (a writer without a real network connection), which must not turn beginning
// a stream into a failure.
func TestBeginStreamToleratesUnsupportedWriteDeadline(t *testing.T) {
	c := web.NewCtx(stubRequestContext{writer: newDeadlineResponseWriter(http.ErrNotSupported)})

	stream, err := c.BeginStream(http.StatusOK, "text/event-stream")
	require.NoError(t, err, "a writer with no deadline support must not fail BeginStream")
	assert.NotNil(t, stream)
}

// TestSSESetsStreamingHeaders pins the three headers SSE must set before the
// response is committed. Setting any of them after the status line would make
// the header invisible to the client, so they are asserted on the committed
// snapshot rather than the recorder's live map: the live map stays mutable
// after WriteHeader, so an assertion on it would pass for a header set too
// late.
func TestSSESetsStreamingHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))

	sse, err := c.SSE()
	require.NoError(t, err)
	require.NotNil(t, sse)

	committed := recorder.Result().Header
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "text/event-stream", committed.Get("Content-Type"))
	assert.Equal(t, "no-cache", committed.Get("Cache-Control"))
	assert.Equal(t, "no", committed.Get("X-Accel-Buffering"),
		"the proxy buffering hint must be in the committed headers")
	assert.True(t, recorder.Flushed)
}

// TestSSEFraming compares the encoder's output against the SSE grammar byte
// for byte. Each case begins a fresh stream, because the framing contract is
// per event and an accumulated body would hide a missing or duplicated
// terminator.
func TestSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		send func(*web.SSE) error
		want string
	}{
		{
			name: "single-line data",
			send: func(s *web.SSE) error { return s.Send("", []byte("hello")) },
			want: "data: hello\n\n",
		},
		{
			name: "multi-line data",
			send: func(s *web.SSE) error { return s.Send("", []byte("first\nsecond")) },
			want: "data: first\ndata: second\n\n",
		},
		{
			name: "event name",
			send: func(s *web.SSE) error { return s.Send("message", []byte("payload")) },
			want: "event: message\ndata: payload\n\n",
		},
		{
			name: "empty data line",
			send: func(s *web.SSE) error { return s.Send("", []byte("")) },
			want: "data: \n\n",
		},
		{
			name: "crlf input line",
			send: func(s *web.SSE) error { return s.Send("", []byte("first\r\nsecond")) },
			want: "data: first\ndata: second\n\n",
		},
		{
			name: "bare CR input line",
			send: func(s *web.SSE) error { return s.Send("", []byte("first\rsecond")) },
			want: "data: first\ndata: second\n\n",
		},
		{
			name: "mixed line terminators",
			send: func(s *web.SSE) error { return s.Send("", []byte("a\r\nb\rc\nd")) },
			want: "data: a\ndata: b\ndata: c\ndata: d\n\n",
		},
		{
			// A lone CR is a line terminator in the SSE grammar, so payload
			// text containing one must not be able to leave the data field:
			// the forged "event: hijack" line stays inside the data payload.
			name: "bare CR cannot inject a field",
			send: func(s *web.SSE) error { return s.Send("", []byte("x\revent: hijack")) },
			want: "data: x\ndata: event: hijack\n\n",
		},
		{
			name: "comment",
			send: func(s *web.SSE) error { return s.Comment("keep-alive") },
			want: ": keep-alive\n\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
			sse, err := c.SSE()
			require.NoError(t, err)

			require.NoError(t, tc.send(sse))
			assert.Equal(t, []byte(tc.want), recorder.Body.Bytes(),
				"the encoded event must match the SSE grammar byte for byte")
		})
	}
}

// TestSSERefusesNilEncoding covers a nil encoder: a handler that lost the
// value must get an error from either method rather than a panic.
func TestSSERefusesNilEncoding(t *testing.T) {
	var sse *web.SSE
	require.Error(t, sse.Send("", []byte("payload")))
	require.Error(t, sse.Comment("keep-alive"))
}

// TestSSERefusesInvalidEventName covers the token rule: a name carrying a
// field separator, whitespace, a control character, or a non-ASCII byte could
// splice protocol syntax into the framing, so it is refused before any byte is
// written.
func TestSSERefusesInvalidEventName(t *testing.T) {
	for _, event := range []string{"bad:event", "bad event", "bad\nname", "bad\rname", "événement"} {
		t.Run(event, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
			sse, err := c.SSE()
			require.NoError(t, err)

			err = sse.Send(event, []byte("payload"))
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "web: SSE "),
				"the refusal must carry the SSE error prefix, got %q", err.Error())
			assert.Empty(t, recorder.Body.String(),
				"a refused event must not write a partial frame")
		})
	}
}

// TestSSERefusesControlCharactersInComment covers the comment rule. A CR or
// LF would end the comment line; the rest of the control range is refused for
// the same reason. The response is already committed by SSE, so the assertion
// is on the body: no partial comment may escape.
func TestSSERefusesControlCharactersInComment(t *testing.T) {
	for _, text := range []string{"line\nbreak", "carriage\rreturn", "tab\there", "nul\x00here", "delete\x7fhere"} {
		t.Run(text, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
			sse, err := c.SSE()
			require.NoError(t, err)

			err = sse.Comment(text)
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "web: SSE "),
				"the refusal must carry the SSE error prefix, got %q", err.Error())
			assert.Empty(t, recorder.Body.String(),
				"a refused comment must not write a partial line")
		})
	}
}
