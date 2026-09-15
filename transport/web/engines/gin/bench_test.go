package gin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// benchCtx builds a Ctx over the real adapter. These benchmarks live in this
// module rather than in transport/web because what they measure is the cost of
// forwarding through a concrete engine; running them against the neutral test
// engine would measure a different implementation.
func benchCtx(b *testing.B) *web.Ctx {
	b.Helper()
	ginlib.SetMode(ginlib.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := ginlib.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, "/bench?k=v", nil)
	gc.Request.Header.Set("X-Bench", "1")
	return web.NewCtx(newRequestContext(gc))
}

func BenchmarkCtxRequest(b *testing.B) {
	c := benchCtx(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Request()
	}
}

func BenchmarkCtxWriter(b *testing.B) {
	c := benchCtx(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Writer()
	}
}

func BenchmarkCtxGetHeader(b *testing.B) {
	c := benchCtx(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.GetHeader("X-Bench")
	}
}

func BenchmarkCtxQuery(b *testing.B) {
	c := benchCtx(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Query("k")
	}
}

func BenchmarkCtxSetGet(b *testing.B) {
	c := benchCtx(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set("k", i)
		_, _ = c.Get("k")
	}
}
