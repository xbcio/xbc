package timeout

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
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
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: 20 * time.Millisecond})
	contextCanceled := make(chan struct{})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/slow", func(c *gin.Context) {
		c.Header("X-Partial", "must-not-leak")
		c.String(http.StatusCreated, "partial response")
		<-c.Request.Context().Done()
		close(contextCanceled)
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
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/fast", func(c *gin.Context) {
		c.Header("X-Test", "yes")
		c.String(http.StatusCreated, "created")
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fast", nil))
	if response.Code != http.StatusCreated || response.Body.String() != "created" || response.Header().Get("X-Test") != "yes" {
		t.Fatalf("response = %d %#v %q", response.Code, response.Header(), response.Body.String())
	}
}

func TestPanicIsRethrownWithoutLeakingBufferedResponse(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/panic", func(c *gin.Context) {
		c.Header("X-Partial", "must-not-leak")
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
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: 10 * time.Millisecond})
	var observedStatus, observedBytes int
	var observedWritten bool
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Next()
		observedStatus = c.Writer.Status()
		observedBytes = c.Writer.Size()
		observedWritten = c.Writer.Written()
	})
	router.Use(p.handle)
	router.GET("/slow", func(c *gin.Context) {
		<-c.Request.Context().Done()
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if observedStatus != http.StatusGatewayTimeout || observedBytes != response.Body.Len() || !observedWritten {
		t.Fatalf("observer saw status=%d bytes=%d written=%v; response=%d/%d", observedStatus, observedBytes, observedWritten, response.Code, response.Body.Len())
	}
}

func TestStreamingUpgradeAndExcludedPathsBypassTimeout(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Millisecond, ExcludePaths: []string{"/jobs/*"}})
	var calls atomic.Int32
	router := gin.New()
	router.Use(p.handle)
	router.Any("/*path", func(c *gin.Context) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		c.String(http.StatusOK, "ok")
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
// into a direct writer.Unwrap() call. gin.ResponseWriter has no Unwrap
// method, so a direct call would stop compiling the moment Unwrap is
// deleted -- the package would fail to build, no test would run at all, and
// that build failure is easily misread as this test catching the deletion.
// Asserting through an interface compiles either way and turns the deletion
// into an honest failed assertion.
func TestTimeoutWriterUnwrapReachesUnderlyingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	original := gc.Writer
	writer := newTimeoutWriter(original)

	unwrapper, ok := any(writer).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("timeoutWriter must expose Unwrap")
	}
	if unwrapper.Unwrap() != http.ResponseWriter(original) {
		t.Fatal("Unwrap must return the writer the wrapper was built around")
	}
}

// TestAbortWithStatusCommitsBufferedStatus pins WriteHeaderNow as
// load-bearing. gin's own Context.AbortWithStatus calls WriteHeaderNow
// directly, and timeoutWriter.WriteHeader only ever records the status into
// its own buffered field -- the embedded writer's status field is never
// touched until commit. If WriteHeaderNow is deleted, the promoted method
// operates on the embedded writer's own (still-default 200) status instead
// of the buffered one, so the real response is sent prematurely with the
// wrong status and the later deferred commit is a no-op superfluous call.
func TestAbortWithStatusCommitsBufferedStatus(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/abort", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusTeapot)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/abort", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTeapot)
	}
}

// TestWriteStringBuffersBodyAndDeferredHeaders pins WriteString as
// load-bearing. gin.ResponseWriter exposes WriteString as a direct part of
// its public contract. If WriteString is deleted, the promoted method
// forwards straight to the embedded writer's own WriteString, which sends
// real headers and body immediately using the embedded writer's own header
// map -- skipping timeoutWriter's header buffer entirely, so a header set
// via c.Header after wrapping never reaches the response actually sent.
// response.Result().Header is used rather than response.Header() because
// httptest.ResponseRecorder.Header() always returns the live, still-mutable
// map; only Result().Header is the frozen snapshot taken at the moment
// headers were actually sent, matching real connection behavior.
func TestWriteStringBuffersBodyAndDeferredHeaders(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/tiny", func(c *gin.Context) {
		c.Header("X-Custom", "yes")
		if _, err := c.Writer.WriteString("tiny"); err != nil {
			t.Fatal(err)
		}
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tiny", nil))
	if response.Result().Header.Get("X-Custom") != "yes" || response.Body.String() != "tiny" {
		t.Fatalf("headers=%v body=%q", response.Result().Header, response.Body.String())
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
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/flush", func(c *gin.Context) {
		c.String(http.StatusOK, "buffered-before-flush")
		c.Writer.Flush()
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
func TestHijackRejectsAfterBufferedWrite(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := pluginFor(t, Config{Duration: time.Second})
	router := gin.New()
	router.Use(p.handle)
	done := make(chan error, 1)
	router.GET("/hijack", func(c *gin.Context) {
		c.String(http.StatusOK, "buffered")
		hijacker, ok := c.Writer.(http.Hijacker)
		if !ok {
			done <- errors.New("writer does not implement http.Hijacker")
			return
		}
		_, _, err := hijacker.Hijack()
		done <- err
	})
	server := httptest.NewServer(router)
	defer server.Close()
	resp, err := http.Get(server.URL + "/hijack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if hijackErr := <-done; hijackErr == nil {
		t.Fatal("Hijack must reject once a buffered response write occurred")
	}
}
