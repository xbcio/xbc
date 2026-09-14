package web

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResponseWriterStaysNarrow pins the neutral contract's width. The
// interface exists to be engine-neutral, and the cheapest way to lose that
// property is for someone to add a gin-shaped method to it because a call
// site happened to need one. WriteHeaderNow and WriteString are gin's own
// two-phase-commit vocabulary; Pusher is gin-specific HTTP/2 push. None of
// them belong here.
//
// If this test goes red, read it as "the neutral writer contract just grew a
// gin-shaped method," not as "update the list below."
func TestResponseWriterStaysNarrow(t *testing.T) {
	writerType := reflect.TypeOf((*ResponseWriter)(nil)).Elem()

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

// TestGinResponseWriterSatisfiesContract is the whole reason this phase is
// behaviourally inert: gin's own writer already implements every method the
// neutral contract asks for, so narrowing Ctx.Writer() changes no runtime
// behaviour. A future gin upgrade that drops one of these methods must fail
// here rather than at an unrelated call site.
func TestGinResponseWriterSatisfiesContract(t *testing.T) {
	ginWriterType := reflect.TypeOf((*gin.ResponseWriter)(nil)).Elem()
	neutralType := reflect.TypeOf((*ResponseWriter)(nil)).Elem()

	require.True(t, ginWriterType.Implements(neutralType),
		"gin.ResponseWriter must satisfy web.ResponseWriter")
}

// Ctx.Writer must return the neutral contract, not gin's. Go function types
// are invariant in their results, so widening the return type back to
// gin.ResponseWriter stops this assignment from compiling. This is the only
// construct that can pin a return type: a runtime assertion cannot tell the
// two apart, because gin.ResponseWriter already satisfies ResponseWriter.
//
// A build failure is the correct and intended signal here, unlike elsewhere in
// this package where a build failure would be a false red.
var _ func(*Ctx) ResponseWriter = (*Ctx).Writer

// TestCtxWriterReportsCommitAndReachesFlusher covers the two capabilities the
// framework's own guards depend on: Written() must flip once a response is
// committed (problem.go and errors.go read it to avoid writing a second
// response over a handler's own output), and Flush() must reach the real
// underlying writer (a buffering wrapper forwards to it).
func TestCtxWriterReportsCommitAndReachesFlusher(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	c := newCtx(gc)

	var writer ResponseWriter = c.Writer()
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
