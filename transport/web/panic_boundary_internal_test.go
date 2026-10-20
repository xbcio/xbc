package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// panicRequestContext is a RequestContext over a caller-supplied request,
// including the two degenerate shapes the boundary must absorb: a nil request
// and a non-nil request whose URL is nil. Next panics so the panic boundary's
// recover path -- not a normal return -- is the code under test.
type panicRequestContext struct {
	request *http.Request
	writer  ResponseWriter
	next    func()
}

func (rc *panicRequestContext) Request() *http.Request     { return rc.request }
func (*panicRequestContext) SetRequest(*http.Request)      {}
func (rc *panicRequestContext) Writer() ResponseWriter     { return rc.writer }
func (rc *panicRequestContext) SetWriter(w ResponseWriter) { rc.writer = w }
func (*panicRequestContext) Param(string) string           { return "" }
func (*panicRequestContext) ClientIP() string              { return "" }
func (*panicRequestContext) Status(int)                    {}
func (*panicRequestContext) Bind(any) error                { return nil }
func (*panicRequestContext) BindURI(any) error             { return nil }
func (*panicRequestContext) JSON(int, any) error           { return nil }
func (rc *panicRequestContext) Next() {
	if rc.next != nil {
		rc.next()
	}
}
func (*panicRequestContext) Abort() {}

// panicRecorder is a ResponseWriter over httptest.ResponseRecorder that tracks
// commitment the way the engine adapters do, so the panic boundary's
// written/not-written branches are both observable from a synthetic Ctx.
type panicRecorder struct {
	*httptest.ResponseRecorder
	written bool
}

func newPanicRecorder() *panicRecorder {
	return &panicRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *panicRecorder) Write(p []byte) (int, error) {
	w.written = true
	return w.ResponseRecorder.Write(p)
}

func (w *panicRecorder) WriteHeader(code int) {
	w.written = true
	w.ResponseRecorder.WriteHeader(code)
}

func (w *panicRecorder) Status() int { return w.ResponseRecorder.Code }

func (w *panicRecorder) Size() int {
	if !w.written {
		return -1
	}
	return w.ResponseRecorder.Body.Len()
}

func (w *panicRecorder) Written() bool { return w.written }

// nilRequestLogger captures the boundary's Warn/Error entries.
type nilRequestLogger struct {
	log.Logger

	mu      sync.Mutex
	entries [][]any
}

func (l *nilRequestLogger) Warn(_ string, kv ...any)  { l.add(kv) }
func (l *nilRequestLogger) Error(_ string, kv ...any) { l.add(kv) }

func (l *nilRequestLogger) add(kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, append([]any(nil), kv...))
}

func (l *nilRequestLogger) fields() []any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == 0 {
		return nil
	}
	return l.entries[0]
}

// TestPanicBoundarySurvivesNilRequest pins the recovery contract for the Ctx
// shape NewCtx explicitly allows: a nil request must degrade the log fields
// instead of making the boundary panic a second time. On the baseline this
// test fails because the recover path dereferences c.Request() directly.
func TestPanicBoundarySurvivesNilRequest(t *testing.T) {
	logger := &nilRequestLogger{Logger: log.Nop()}
	writer := newPanicRecorder()
	guard := newPanicBoundary(logger, false)
	c := NewCtx(&panicRequestContext{
		writer: writer,
		next:   func() { panic("downstream handler panicked") },
	})

	require.NotPanics(t, func() {
		require.NoError(t, guard.handle(context.Background(), c))
	}, "the panic boundary must not panic while recovering on a nil-request Ctx")

	fields := logger.fields()
	require.NotEmpty(t, fields, "the recovered panic must still be logged")
	assert.Equal(t, "", fieldOf(fields, "method"),
		"a nil request must degrade the method field to an empty value")
	assert.Equal(t, "", fieldOf(fields, "path"),
		"a nil request must degrade the path field to an empty value")

	assert.Equal(t, http.StatusInternalServerError, writer.Status(),
		"a nil-request panic must still produce a 500 Problem Detail")
	assert.Contains(t, writer.Body.String(), "internal_server_error")
}

// TestPanicBoundarySurvivesRequestWithoutURL pins the second degraded shape: a
// non-nil request whose URL is nil. Without the URL guard the recover path
// dereferences it and panics a second time, so this case is what keeps that
// guard alive.
func TestPanicBoundarySurvivesRequestWithoutURL(t *testing.T) {
	logger := &nilRequestLogger{Logger: log.Nop()}
	writer := newPanicRecorder()
	guard := newPanicBoundary(logger, false)
	c := NewCtx(&panicRequestContext{
		request: &http.Request{Method: http.MethodGet},
		writer:  writer,
		next:    func() { panic("downstream handler panicked") },
	})

	require.NotPanics(t, func() {
		require.NoError(t, guard.handle(context.Background(), c))
	}, "the panic boundary must not panic while recovering on a request without a URL")

	fields := logger.fields()
	require.NotEmpty(t, fields, "the recovered panic must still be logged")
	assert.Equal(t, http.MethodGet, fieldOf(fields, "method"),
		"a present request must keep contributing its method")
	assert.Equal(t, "", fieldOf(fields, "path"),
		"a nil URL must degrade the path field to an empty value")

	assert.Equal(t, http.StatusInternalServerError, writer.Status(),
		"a URL-less panic must still produce a 500 Problem Detail")
	assert.Contains(t, writer.Body.String(), "internal_server_error")
}

// TestPanicBoundarySurvivesNilWriter pins the third degenerate Ctx shape the
// recover path must absorb: a RequestContext whose Writer() is nil. Logging the
// recovered panic must classify the response as uncommitted without
// dereferencing the nil writer, and the 500 Problem Detail write is skipped
// because there is no writer to carry it -- WriteProblem already tolerates a
// nil writer. Without the nil guard this test fails with a second panic.
func TestPanicBoundarySurvivesNilWriter(t *testing.T) {
	logger := &nilRequestLogger{Logger: log.Nop()}
	guard := newPanicBoundary(logger, false)
	c := NewCtx(&panicRequestContext{
		writer: nil,
		next:   func() { panic("downstream handler panicked") },
	})

	require.NotPanics(t, func() {
		require.NoError(t, guard.handle(context.Background(), c))
	}, "the panic boundary must not panic while recovering on a nil-writer Ctx")

	fields := logger.fields()
	require.NotEmpty(t, fields, "the recovered panic must still be logged")
	assert.Equal(t, false, fieldOf(fields, "response_written"),
		"a nil writer must be treated as an uncommitted response")

	assert.Nil(t, c.Writer(),
		"no writer exists to receive a 500 Problem Detail, and none may be installed")
}

// fieldOf reads a key/value field pair from a log entry, returning nil when
// the key was never recorded.
func fieldOf(fields []any, key string) any {
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == key {
			return fields[i+1]
		}
	}
	return nil
}
