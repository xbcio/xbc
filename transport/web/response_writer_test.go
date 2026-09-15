package web_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestResponseWriterStaysNarrow pins the neutral contract's width. The
// interface exists to be engine-neutral, and the cheapest way to lose that
// property is for someone to add an engine-shaped method to it because a call
// site happened to need one. WriteHeaderNow and WriteString are gin's own
// two-phase-commit vocabulary; Pusher is gin-specific HTTP/2 push. None of
// them belong here.
//
// If this test goes red, read it as "the neutral writer contract just grew an
// engine-shaped method," not as "update the list below."
func TestResponseWriterStaysNarrow(t *testing.T) {
	writerType := reflect.TypeOf((*web.ResponseWriter)(nil)).Elem()

	allowed := map[string]bool{
		"Header": true, "Write": true, "WriteHeader": true,
		"Flush": true, "Status": true, "Size": true, "Written": true,
	}
	for i := range writerType.NumMethod() {
		name := writerType.Method(i).Name
		assert.True(t, allowed[name], "unexpected method %s on ResponseWriter", name)
	}
	assert.Equal(t, len(allowed), writerType.NumMethod())
}

// Ctx.Writer must return the neutral contract, not an engine's own writer type.
// Go function types are invariant in their results, so widening the return type
// back to a concrete engine writer stops this assignment from compiling. This is
// the only construct that can pin a return type: a runtime assertion cannot tell
// the two apart, because an engine writer that satisfies ResponseWriter is
// indistinguishable from ResponseWriter at run time.
//
// A build failure is the correct and intended signal here.
var _ func(*web.Ctx) web.ResponseWriter = (*web.Ctx).Writer

// TestCtxWriterReportsCommitAndReachesFlusher covers the two capabilities the
// framework's own guards depend on: Written() must flip once a response is
// committed (problem.go and errors.go read it to avoid writing a second
// response over a handler's own output), and Flush() must reach the real
// underlying writer (a buffering wrapper forwards to it).
func TestCtxWriterReportsCommitAndReachesFlusher(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	var writer web.ResponseWriter = c.Writer()
	require.NotNil(t, writer)
	assert.False(t, writer.Written(), "nothing written yet")

	_, err := writer.Write([]byte("payload"))
	require.NoError(t, err)
	assert.True(t, writer.Written(), "Written must flip once bytes are committed")
	assert.Equal(t, len("payload"), writer.Size())

	writer.Flush()
	assert.True(t, recorder.Flushed, "Flush must reach the underlying writer")
	assert.Equal(t, "payload", recorder.Body.String())
}
