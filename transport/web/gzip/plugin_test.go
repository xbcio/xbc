package gzip

import (
	"bytes"
	compressgzip "compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/timeout"
)

func TestDefinitionAndOrderingContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseBusiness || len(order.After) != 1 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
	if ref := order.After[0]; ref.Key() != timeout.Key || ref.InstanceName() != "" || ref.Required() {
		t.Fatalf("timeout order reference = %#v, want optional timeout key", ref)
	}
}

func TestCompressesNegotiatedCompressibleResponse(t *testing.T) {
	body := strings.Repeat("compress me ", 300)
	response := perform(t, DefaultConfig(), http.MethodGet, "/data", map[string]string{"Accept-Encoding": "br, gzip;q=0.8"}, func(c *gin.Context) {
		c.Header("ETag", `"strong"`)
		c.Data(http.StatusOK, "application/json", []byte(body))
	})
	if response.Header().Get("Content-Encoding") != "gzip" || !headerContainsToken(response.Header().Values("Vary"), "Accept-Encoding") {
		t.Fatalf("compression headers = %#v", response.Header())
	}
	if response.Header().Get("ETag") != `W/"strong"` {
		t.Fatalf("ETag = %q", response.Header().Get("ETag"))
	}
	if got := ungzip(t, response.Body.Bytes()); got != body {
		t.Fatalf("decoded body mismatch: %d bytes", len(got))
	}
}

func TestSkipsSmallSSEUpgradeEncodedRangeAndNoTransformResponses(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		headers map[string]string
		handler gin.HandlerFunc
	}{
		{"small", "/small", nil, func(c *gin.Context) { c.String(http.StatusOK, "tiny") }},
		{"sse", "/sse", map[string]string{"Accept": "text/event-stream"}, func(c *gin.Context) {
			c.Data(http.StatusOK, "text/event-stream", []byte(strings.Repeat("data: x\n\n", 200)))
		}},
		{"upgrade", "/upgrade", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}, func(c *gin.Context) { c.String(http.StatusOK, strings.Repeat("x", 2000)) }},
		{"encoded", "/encoded", nil, func(c *gin.Context) {
			c.Header("Content-Encoding", "br")
			c.String(http.StatusOK, strings.Repeat("x", 2000))
		}},
		{"range", "/range", map[string]string{"Range": "bytes=0-100"}, func(c *gin.Context) { c.String(http.StatusPartialContent, strings.Repeat("x", 2000)) }},
		{"no-transform", "/no-transform", nil, func(c *gin.Context) {
			c.Header("Cache-Control", "public, no-transform")
			c.String(http.StatusOK, strings.Repeat("x", 2000))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{"Accept-Encoding": "gzip"}
			for key, value := range tt.headers {
				headers[key] = value
			}
			response := perform(t, DefaultConfig(), http.MethodGet, tt.path, headers, tt.handler)
			if got := response.Header().Get("Content-Encoding"); got == "gzip" {
				t.Fatalf("response unexpectedly gzip encoded: %#v", response.Header())
			}
		})
	}
}

func TestAcceptEncodingQualityAndExcludedPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinLength = 0
	cfg.ExcludePaths = []string{"/stream/*"}
	for _, tc := range []struct {
		path, accept string
	}{
		{"/data", "gzip;q=0"},
		{"/data", "br"},
		{"/stream/jobs", "gzip"},
	} {
		response := perform(t, cfg, http.MethodGet, tc.path, map[string]string{"Accept-Encoding": tc.accept}, func(c *gin.Context) {
			c.String(http.StatusOK, strings.Repeat("x", 100))
		})
		if got := response.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s %q encoded as %q", tc.path, tc.accept, got)
		}
	}
}

func TestIdentityAndHEADResponsesStillVaryOnAcceptEncoding(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinLength = 0
	for _, tc := range []struct {
		name, method, accept string
	}{
		{name: "identity", method: http.MethodGet, accept: "identity"},
		{name: "gzip-disabled", method: http.MethodGet, accept: "gzip;q=0"},
		{name: "head", method: http.MethodHead, accept: "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := perform(t, cfg, tc.method, "/data", map[string]string{"Accept-Encoding": tc.accept}, func(c *gin.Context) {
				c.String(http.StatusOK, strings.Repeat("x", 100))
			})
			if response.Header().Get("Content-Encoding") != "" || !headerContainsToken(response.Header().Values("Vary"), "Accept-Encoding") {
				t.Fatalf("headers = %#v", response.Header())
			}
		})
	}
}

func TestPanicDoesNotCommitBufferedBody(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	cfg := DefaultConfig()
	cfg.MinLength = 0
	p := New()
	state, _ := normalizeConfig(cfg)
	p.state.Store(&state)
	router := gin.New()
	router.Use(p.handle)
	router.GET("/panic", func(c *gin.Context) {
		c.Header("X-Partial", "must-not-leak")
		c.String(http.StatusOK, "secret partial body")
		panic("boom")
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		router.ServeHTTP(response, request)
	}()
	if response.Body.Len() != 0 || response.Header().Get("Content-Encoding") != "" || response.Header().Get("X-Partial") != "" {
		t.Fatalf("panic committed buffered response: headers=%v body=%q", response.Header(), response.Body.String())
	}
}

func perform(t *testing.T, cfg Config, method, path string, headers map[string]string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	p := New()
	state, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.state.Store(&state)
	router := gin.New()
	router.Use(p.handle)
	router.Handle(method, path, handler)
	request := httptest.NewRequest(method, path, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func ungzip(t *testing.T, encoded []byte) string {
	t.Helper()
	reader, err := compressgzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}
