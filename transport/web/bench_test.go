package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func benchCtx(b *testing.B) *Ctx {
	b.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, "/bench?k=v", nil)
	gc.Request.Header.Set("X-Bench", "1")
	return newCtx(gc)
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
