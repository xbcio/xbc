package timeout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestDefinitionAndOrderingContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseBusiness || len(order.Before) != 0 || len(order.After) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestReturnsClean504AndCancelsRequestContext(t *testing.T) {
	p := pluginFor(t, Config{Duration: 20 * time.Millisecond})
	contextCanceled := make(chan struct{})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/slow", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Partial", "must-not-leak")
		c.String(http.StatusCreated, "partial response")
		<-c.Request().Context().Done()
		close(contextCanceled)
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != http.StatusGatewayTimeout || problem.Properties["code"] != "gateway_timeout" || problem.Instance != "/slow" ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("problem = %#v headers=%v", problem, response.Header())
	}
	if response.Header().Get("X-Partial") != "" || strings.Contains(response.Body.String(), "partial") {
		t.Fatalf("partial response leaked: headers=%v body=%q", response.Header(), response.Body.String())
	}
	select {
	case <-contextCanceled:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled")
	}
}

func TestFastResponseCommitsExactlyOnce(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/fast", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Test", "yes")
		c.String(http.StatusCreated, "created")
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fast", nil))
	if response.Code != http.StatusCreated || response.Body.String() != "created" || response.Header().Get("X-Test") != "yes" {
		t.Fatalf("response = %d %#v %q", response.Code, response.Header(), response.Body.String())
	}
}

func TestPanicIsRethrownWithoutLeakingBufferedResponse(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/panic", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Partial", "must-not-leak")
		c.String(http.StatusCreated, "secret partial body")
		panic("boom")
	})
	response := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	if response.Body.Len() != 0 || response.Header().Get("X-Partial") != "" {
		t.Fatalf("panic leaked buffered response: headers=%v body=%q", response.Header(), response.Body.String())
	}
}

func TestOuterObserverSeesFinalTimeoutStatusAndBytes(t *testing.T) {
	p := pluginFor(t, Config{Duration: 10 * time.Millisecond})
	var observedStatus, observedBytes int
	var observedWritten bool
	router := enginetest.New()
	router.Use(func(_ context.Context, c *web.Ctx) error {
		c.Next()
		observedStatus = c.Writer().Status()
		observedBytes = c.Writer().Size()
		observedWritten = c.Writer().Written()
		return nil
	})
	router.Use(p.handle)
	router.GET("/slow", func(_ context.Context, c *web.Ctx) error {
		<-c.Request().Context().Done()
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if observedStatus != http.StatusGatewayTimeout || observedBytes != response.Body.Len() || !observedWritten {
		t.Fatalf("observer saw status=%d bytes=%d written=%v; response=%d/%d", observedStatus, observedBytes, observedWritten, response.Code, response.Body.Len())
	}
}

func TestStreamingUpgradeAndExcludedPathsBypassTimeout(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Millisecond, ExcludePaths: []string{"/jobs/*"}})
	var calls atomic.Int32
	router := enginetest.New()
	router.Use(p.handle)
	// "GET /" is ServeMux's catch-all for this method, which is all these
	// cases need; the engine's own method-less catch-all stays out of the way.
	router.GET("/", func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		c.String(http.StatusOK, "ok")
		return nil
	})
	tests := []struct {
		path    string
		headers map[string]string
	}{
		{"/events", map[string]string{"Accept": "text/event-stream"}},
		{"/socket", map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "websocket"}},
		{"/jobs/42", nil},
	}
	for _, tt := range tests {
		request := httptest.NewRequest(http.MethodGet, tt.path, nil)
		for key, value := range tt.headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != "ok" {
			t.Fatalf("%s was not bypassed: %d %q", tt.path, response.Code, response.Body.String())
		}
	}
	if calls.Load() != int32(len(tests)) {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestExcludedRouteNameMatcher(t *testing.T) {
	cfg, err := normalizeConfig(Config{Duration: time.Second, ExcludeRoutes: []string{"stream.events"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.excludeRoutes["stream.events"]; !ok {
		t.Fatal("route name was not compiled")
	}
	if cfg.excludedPath("/stream/events") {
		t.Fatal("route names must not become path rules")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Duration: time.Second, ExcludePaths: []string{"relative"}},
		{Duration: time.Second, ExcludeRoutes: []string{" "}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", cfg)
		}
	}
}

func pluginFor(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	if cfg.Duration == 0 {
		cfg.Duration = time.Second
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	p.state.Store(&runtimeState{config: normalized})
	return p
}

// TestTimeoutWriterUnwrapReachesUnderlyingWriter pins Unwrap as load-bearing
// rather than boilerplate. http.ResponseController follows Unwrap() to find
// the real writer, and that is the route Hijack takes once the response
// contract no longer exposes Hijack directly. Without this test Unwrap could
// be deleted while every other test stayed green.
//
// The interface assertion below is deliberate and must not be "simplified"
// into a direct writer.Unwrap() call. web.ResponseWriter has no Unwrap
// method, so a direct call would stop compiling the moment Unwrap is
// deleted -- the package would fail to build, no test would run at all, and
// that build failure is easily misread as this test catching the deletion.
// Asserting through an interface compiles either way and turns the deletion
// into an honest failed assertion.
func TestTimeoutWriterUnwrapReachesUnderlyingWriter(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	original := c.Writer()
	writer := newTimeoutWriter(original)

	unwrapper, ok := any(writer).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("timeoutWriter must expose Unwrap")
	}
	if unwrapper.Unwrap() != http.ResponseWriter(original) {
		t.Fatal("Unwrap must return the writer the wrapper was built around")
	}
}

// TestAbortedRequestStillCommitsTheBufferedStatus pins the deferred commit for
// the chain that ends early. A handler that aborts after recording a status
// writes no body at all, so the only thing that can carry 418 to the
// connection is commit's own WriteHeader on the writer beneath -- delete that
// call and the response goes out with the default 200.
//
// This replaces a test that pinned gin's WriteHeaderNow. That method was part
// of gin.ResponseWriter's two-phase face; the wrapper now implements the
// neutral web.ResponseWriter, which has no such method, so what is left to pin
// is the commit itself.
func TestAbortedRequestStillCommitsTheBufferedStatus(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/abort", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusTeapot)
		c.Abort()
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/abort", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTeapot)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("aborted handler wrote a body: %q", response.Body.String())
	}
}

// TestWriteBuffersBodyAndDeferredHeaders pins Write and Header as
// load-bearing overrides. If either is deleted, the promoted method forwards
// straight to the embedded writer, which sends real headers and body
// immediately using its own header map -- skipping this wrapper's buffer
// entirely, so a header set after wrapping never reaches the response actually
// sent and a timed-out request leaks the partial body it was supposed to
// discard. response.Result().Header is used rather than response.Header()
// because httptest.ResponseRecorder.Header() always returns the live,
// still-mutable map; only Result().Header is the frozen snapshot taken at the
// moment headers were actually sent, matching real connection behavior.
//
// The write itself always lands in the body buffer regardless of whether
// Write marks the writer written -- timeoutWriter.commit copies w.body
// unconditionally, so the header/body assertions above pass even if the
// w.written assignment inside Write is deleted. The observedWritten and
// observedSize checks below read c.Writer().Written()/Size() while the wrapper
// is still installed, which only report the write once Write has set
// w.written -- the same commit-state guard transport/web/problem.go,
// errors.go, extensions/response/biz and extensions/reliability/recovery rely
// on to avoid writing a response twice.
func TestWriteBuffersBodyAndDeferredHeaders(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	var observedWritten bool
	var observedSize int
	router.GET("/tiny", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Custom", "yes")
		if _, err := c.Writer().Write([]byte("tiny")); err != nil {
			t.Fatal(err)
		}
		observedWritten = c.Writer().Written()
		observedSize = c.Writer().Size()
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tiny", nil))
	if response.Result().Header.Get("X-Custom") != "yes" || response.Body.String() != "tiny" {
		t.Fatalf("headers=%v body=%q", response.Result().Header, response.Body.String())
	}
	if !observedWritten || observedSize != len("tiny") {
		t.Fatalf("timeoutWriter did not record commit state after Write: written=%v size=%d", observedWritten, observedSize)
	}
}

// TestStatusAndSizeReportBufferedStateBeforeCommit pins Status and Size as
// load-bearing. Both are embedded-method overrides, so deleting either one
// still compiles and still satisfies the web.ResponseWriter assertion at the
// bottom of middleware.go -- the call is simply promoted to the embedded
// writer, which answers about its own untouched state rather than about the
// buffer.
//
// Neither method can be pinned from outside this middleware.
// TestOuterObserverSeesFinalTimeoutStatusAndBytes reads both from an outer
// middleware, but that runs after the deferred restore, so it reads the real
// writer post-commit and sees the same values either way; the same is true of
// every production reader (metrics, tracing, accesslog, auditlog). The
// observations below are therefore taken from inside the handler, while the
// wrapper is still installed.
//
// Status: c.Status only records the code into the wrapper's own w.status
// field, because timeoutWriter.WriteHeader deliberately does not touch the
// embedded writer, which stays at its default 200. Reading 418 back through
// c.Writer.Status() is therefore only possible while Status is overridden --
// delete it and the promoted method reports the embedded writer's 200. The
// observedWritten check guards that argument rather than repeating coverage:
// the status read only proves something because the status is still buffered
// and uncommitted, since a committed status would have reached the embedded
// writer too and let the mutation survive.
//
// Size: web.ResponseWriter documents -1 as "no write has happened yet", which accesslog and
// auditlog both normalize to 0. The first observation is taken before any
// write, so it pins the sentinel branch itself rather than just the byte
// count -- rewriting that branch to return 0 fails this assertion while every
// byte-count assertion elsewhere in the package still passes.
func TestStatusAndSizeReportBufferedStateBeforeCommit(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	var observedInitialSize, observedStatus int
	var observedWritten bool
	router.GET("/status", func(_ context.Context, c *web.Ctx) error {
		observedInitialSize = c.Writer().Size()
		c.Status(http.StatusTeapot)
		observedStatus = c.Writer().Status()
		observedWritten = c.Writer().Written()
		return nil
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))
	if observedInitialSize != -1 {
		t.Fatalf("timeoutWriter reported size %d before any write, want the -1 sentinel", observedInitialSize)
	}
	if observedWritten {
		t.Fatal("c.Status must leave the response uncommitted, or the status assertion below proves nothing")
	}
	if observedStatus != http.StatusTeapot {
		t.Fatalf("timeoutWriter reported status %d while buffering, want %d", observedStatus, http.StatusTeapot)
	}
}

// TestFlushCommitsBufferedBodyBeforeStreaming pins Flush as load-bearing.
// timeoutWriter.Flush commits whatever is already buffered before falling
// back to passthrough streaming, so a handler that flushes and then panics
// still has its pre-flush output reach the client. If Flush is deleted, the
// promoted method only calls the embedded writer's real Flush -- it never
// commits the buffer -- so a subsequent panic (which skips the deferred
// commit) leaves nothing written at all.
func TestFlushCommitsBufferedBodyBeforeStreaming(t *testing.T) {
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/flush", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "buffered-before-flush")
		c.Writer().Flush()
		panic("boom")
	})
	response := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/flush", nil))
	}()
	if response.Body.String() != "buffered-before-flush" {
		t.Fatalf("flush did not commit buffered body before streaming began: body=%q", response.Body.String())
	}
}

// TestHijackRejectsAfterBufferedWrite pins Hijack as load-bearing. Once a
// response write has been buffered, letting the caller take over the raw
// connection would abandon that buffered data with no way to send it, so
// timeoutWriter.Hijack refuses. If Hijack is deleted, the promoted method
// forwards straight to the embedded writer's real Hijack with no such check,
// silently allowing the takeover.
//
// A real net/http server is used (rather than httptest.ResponseRecorder,
// which does not implement http.Hijacker at all) so the !ok branch below is
// unreachable in the passing case: the wrapper must genuinely implement
// http.Hijacker and genuinely refuse. The !ok branch and the genuine
// rejection both feed the same channel, so the final assertion checks the
// wrapper's exact rejection text (read from timeoutWriter.Hijack in
// middleware.go) rather than merely "err != nil".
// Re-running this mutation must pass an explicit -timeout (this package's
// default is 10 minutes): the deleted-guard mutation lets Hijack succeed on
// the real connection, and since the handler never uses it, the client's
// http.Get blocks forever waiting for a response, so the mutation is killed
// by a timeout rather than a conventional test failure.
func TestHijackRejectsAfterBufferedWrite(t *testing.T) {
	const wantRejection = "timeout: cannot hijack after a buffered response write"
	p := pluginFor(t, Config{Duration: time.Second})
	router := enginetest.New()
	router.Use(p.handle)
	done := make(chan error, 1)
	router.GET("/hijack", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "buffered")
		hijacker, ok := c.Writer().(http.Hijacker)
		if !ok {
			done <- fmt.Errorf("writer %T does not implement http.Hijacker", c.Writer())
			return nil
		}
		_, _, err := hijacker.Hijack()
		done <- err
		return nil
	})
	server := httptest.NewServer(router)
	defer server.Close()
	resp, err := http.Get(server.URL + "/hijack")
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
