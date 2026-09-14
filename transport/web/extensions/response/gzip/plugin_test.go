package gzip

import (
	"bytes"
	compressgzip "compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
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
	router.Use(web.Handle(p.handle))
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
	router.Use(web.Handle(p.handle))
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

// TestBufferingWriterUnwrapReachesUnderlyingWriter pins Unwrap as
// load-bearing rather than boilerplate. http.ResponseController follows
// Unwrap() to find the real writer, and that is the route Hijack takes once
// the response contract no longer exposes Hijack directly. Without this test
// Unwrap could be deleted while every other test stayed green.
//
// The interface assertion below is deliberate and must not be "simplified"
// into a direct writer.Unwrap() call. gin.ResponseWriter has no Unwrap
// method, so a direct call would stop compiling the moment Unwrap is
// deleted -- the package would fail to build, no test would run at all, and
// that build failure is easily misread as this test catching the deletion.
// Asserting through an interface compiles either way and turns the deletion
// into an honest failed assertion.
func TestBufferingWriterUnwrapReachesUnderlyingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	original := gc.Writer
	writer := newBufferingWriter(original)

	unwrapper, ok := any(writer).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("bufferingWriter must expose Unwrap")
	}
	if unwrapper.Unwrap() != http.ResponseWriter(original) {
		t.Fatal("Unwrap must return the writer the wrapper was built around")
	}
}

// TestAbortWithStatusCommitsBufferedStatus pins WriteHeaderNow as
// load-bearing. gin's own Context.AbortWithStatus calls WriteHeaderNow
// directly, and bufferingWriter.WriteHeader only ever records the status
// into its own buffered field -- the embedded writer's status field is never
// touched until commit. If WriteHeaderNow is deleted, the promoted method
// operates on the embedded writer's own (still-default 200) status instead
// of the buffered one, so the real response is sent prematurely with the
// wrong status and the later deferred commit is a no-op superfluous call.
//
// Asserting only the final response.Code is not enough: bufferingWriter.finish
// always commits w.status onto the real writer regardless of whether
// WriteHeaderNow ran, so response.Code stays 418 even if WriteHeaderNow's body
// is emptied out and never marks the writer written. The observedWritten and
// observedSize checks below read c.Writer.Written()/Size() while the wrapper
// is still installed (before the deferred restore swaps c.Writer back to the
// original writer) -- those two proxy methods return w.written/w.body.Len()
// only once WriteHeaderNow has set w.written, which is the exact commit-state
// guard that transport/web/problem.go, errors.go, extensions/response/biz and
// extensions/reliability/recovery rely on to avoid writing a response twice.
func TestAbortWithStatusCommitsBufferedStatus(t *testing.T) {
	var observedWritten bool
	var observedSize int
	response := perform(t, DefaultConfig(), http.MethodGet, "/abort", map[string]string{"Accept-Encoding": "gzip"}, func(c *gin.Context) {
		c.AbortWithStatus(http.StatusTeapot)
		observedWritten = c.Writer.Written()
		observedSize = c.Writer.Size()
	})
	if response.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTeapot)
	}
	if !observedWritten || observedSize == -1 {
		t.Fatalf("bufferingWriter did not record commit state after WriteHeaderNow: written=%v size=%d", observedWritten, observedSize)
	}
}

// TestWriteStringBuffersBodyAndDeferredHeaders pins WriteString as
// load-bearing. gin.ResponseWriter exposes WriteString as a direct part of
// its public contract (gin's own render path in this version happens to use
// plain Write instead, but the interface method remains a documented,
// directly callable capability). If WriteString is deleted, the promoted
// method forwards straight to the embedded writer's own WriteString, which
// sends real headers and body immediately using the embedded writer's own
// header map -- skipping bufferingWriter's header buffer entirely, so a
// header set via c.Header after wrapping never reaches the response actually
// sent. response.Result().Header is used rather than response.Header()
// because httptest.ResponseRecorder.Header() always returns the live,
// still-mutable map; only Result().Header is the frozen snapshot taken at
// the moment headers were actually sent, matching real connection behavior.
//
// The write itself always lands in the body buffer regardless of whether
// WriteString marks the writer written -- bufferingWriter.commit copies
// w.body unconditionally, so the header/body assertions above pass even if
// the w.written assignment inside WriteString is deleted. The
// observedWritten and observedSize checks below read c.Writer.Written()/
// Size() while the wrapper is still installed, which only report the write
// once WriteString has set w.written -- the same commit-state guard other
// packages rely on to avoid writing a response twice.
func TestWriteStringBuffersBodyAndDeferredHeaders(t *testing.T) {
	var observedWritten bool
	var observedSize int
	response := perform(t, DefaultConfig(), http.MethodGet, "/tiny", map[string]string{"Accept-Encoding": "gzip"}, func(c *gin.Context) {
		c.Header("X-Custom", "yes")
		if _, err := c.Writer.WriteString("tiny"); err != nil {
			t.Fatal(err)
		}
		observedWritten = c.Writer.Written()
		observedSize = c.Writer.Size()
	})
	if response.Result().Header.Get("X-Custom") != "yes" || response.Body.String() != "tiny" {
		t.Fatalf("headers=%v body=%q", response.Result().Header, response.Body.String())
	}
	if !observedWritten || observedSize != len("tiny") {
		t.Fatalf("bufferingWriter did not record commit state after WriteString: written=%v size=%d", observedWritten, observedSize)
	}
}

// TestStatusAndSizeReportBufferedStateBeforeCommit pins Status and Size as
// load-bearing. Both are embedded-method overrides, so deleting either one
// still compiles and still satisfies the gin.ResponseWriter assertion at the
// bottom of middleware.go -- the call is simply promoted to the embedded
// writer, which answers about its own untouched state rather than about the
// buffer.
//
// Neither method can be pinned from outside this middleware. Every production
// reader of Status/Size (metrics, tracing, accesslog, auditlog) runs in a
// phase where the wrapper is already gone, so it reads the real writer after
// commit and sees the same value either way. The observations below are
// therefore taken from inside the handler, while the wrapper is still
// installed and before the deferred restore swaps c.Writer back to the
// original writer.
//
// Status: c.Status only records the code into the wrapper's own w.status
// field, because bufferingWriter.WriteHeader deliberately does not touch the
// embedded writer, which stays at its default 200. Reading 418 back through
// c.Writer.Status() is therefore only possible while Status is overridden --
// delete it and the promoted method reports the embedded writer's 200. The
// observedWritten check guards that argument rather than repeating coverage:
// the status read only proves something because the status is still buffered
// and uncommitted, since a committed status would have reached the embedded
// writer too and let the mutation survive.
//
// Size: gin documents -1 as "no write has happened yet", which accesslog and
// auditlog both normalize to 0. The first observation is taken before any
// write, so it pins the sentinel branch itself rather than just the byte
// count -- rewriting that branch to return 0 fails this assertion while every
// byte-count assertion elsewhere in the package still passes.
func TestStatusAndSizeReportBufferedStateBeforeCommit(t *testing.T) {
	var observedInitialSize, observedStatus int
	var observedWritten bool
	perform(t, DefaultConfig(), http.MethodGet, "/status", map[string]string{"Accept-Encoding": "gzip"}, func(c *gin.Context) {
		observedInitialSize = c.Writer.Size()
		c.Status(http.StatusTeapot)
		observedStatus = c.Writer.Status()
		observedWritten = c.Writer.Written()
	})
	if observedInitialSize != -1 {
		t.Fatalf("bufferingWriter reported size %d before any write, want the -1 sentinel", observedInitialSize)
	}
	if observedWritten {
		t.Fatal("c.Status must leave the response uncommitted, or the status assertion below proves nothing")
	}
	if observedStatus != http.StatusTeapot {
		t.Fatalf("bufferingWriter reported status %d while buffering, want %d", observedStatus, http.StatusTeapot)
	}
}

// TestFlushCommitsBufferedBodyBeforeStreaming pins Flush as load-bearing.
// bufferingWriter.Flush commits whatever is already buffered before falling
// back to passthrough streaming, so a handler that flushes and then panics
// still has its pre-flush output reach the client. If Flush is deleted, the
// promoted method only calls the embedded writer's real Flush -- it never
// commits the buffer -- so a subsequent panic (which skips the deferred
// finish call) leaves nothing written at all.
func TestFlushCommitsBufferedBodyBeforeStreaming(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	cfg := DefaultConfig()
	p := New()
	state, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.state.Store(&state)
	router := gin.New()
	router.Use(web.Handle(p.handle))
	router.GET("/flush", func(c *gin.Context) {
		c.String(http.StatusOK, "buffered-before-flush")
		c.Writer.Flush()
		panic("boom")
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/flush", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		router.ServeHTTP(response, request)
	}()
	if response.Body.String() != "buffered-before-flush" {
		t.Fatalf("flush did not commit buffered body before streaming began: body=%q", response.Body.String())
	}
}

// TestHijackRejectsAfterBufferedWrite pins Hijack as load-bearing. Once a
// response write has been buffered, letting the caller take over the raw
// connection would abandon that buffered data with no way to send it, so
// bufferingWriter.Hijack refuses. If Hijack is deleted, the promoted method
// forwards straight to the embedded writer's real Hijack with no such check,
// silently allowing the takeover.
//
// A real net/http server is used (rather than httptest.ResponseRecorder,
// which does not implement http.Hijacker at all) so the !ok branch below is
// unreachable in the passing case: the wrapper must genuinely implement
// http.Hijacker and genuinely refuse. The request sets Accept-Encoding
// explicitly rather than relying on net/http.Transport's automatic
// "Accept-Encoding: gzip" header, because that automatic header is exactly
// what makes this middleware wrap the writer in the first place -- leaving it
// implicit would hide a real precondition of the test.
//
// The !ok branch and the genuine rejection both feed the same channel, so the
// final assertion checks the wrapper's exact rejection text (read from
// bufferingWriter.Hijack in middleware.go) rather than merely "err != nil".
// Re-running this mutation must pass an explicit -timeout (this package's
// default is 10 minutes): the deleted-guard mutation lets Hijack succeed on
// the real connection, and since the handler never uses it, the client's
// http.Get blocks forever waiting for a response, so the mutation is killed
// by a timeout rather than a conventional test failure.
func TestHijackRejectsAfterBufferedWrite(t *testing.T) {
	const wantRejection = "gzip: cannot hijack after a buffered response write"
	gin.SetMode(gin.ReleaseMode)
	cfg := DefaultConfig()
	p := New()
	state, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.state.Store(&state)
	router := gin.New()
	router.Use(web.Handle(p.handle))
	done := make(chan error, 1)
	router.GET("/hijack", func(c *gin.Context) {
		c.String(http.StatusOK, "buffered")
		hijacker, ok := c.Writer.(http.Hijacker)
		if !ok {
			done <- fmt.Errorf("writer %T does not implement http.Hijacker", c.Writer)
			return
		}
		_, _, err := hijacker.Hijack()
		done <- err
	})
	server := httptest.NewServer(router)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/hijack", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	hijackErr := <-done
	if hijackErr == nil {
		t.Fatal("Hijack must reject once a buffered response write occurred")
	}
	if hijackErr.Error() != wantRejection {
		t.Fatalf("Hijack rejected for the wrong reason: got %q, want %q", hijackErr.Error(), wantRejection)
	}
}
