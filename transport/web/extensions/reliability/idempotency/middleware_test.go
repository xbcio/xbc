package idempotency

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

func initializedPlugin(t *testing.T, configure func(*Config), opts ...Option) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	p, err := New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func engineFor(p *Plugin, route web.RouteInfo, handler gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, route)
		web.SetPrincipal(web.NewCtx(c), web.Principal{Subject: "alice", AuthMethod: "test"})
		c.Next()
	})
	engine.Use(web.Handle(p.Handler()))
	engine.Handle(route.Method, route.Path, handler)
	return engine
}

func perform(engine http.Handler, key, body string) *httptest.ResponseRecorder {
	return performPath(engine, "/orders", key, body)
}

func performPath(engine http.Handler, requestPath, key, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestCompletedResponseIsSafelyReplayed(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(c *gin.Context) {
		calls.Add(1)
		data, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusCreated, "application/json", append([]byte(`{"body":`), append(data, '}')...))
	})
	first := perform(engine, "order-123456", `"one"`)
	second := perform(engine, "order-123456", `"one"`)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("first=%d %q second=%d %q replay=%q calls=%d", first.Code, first.Body.String(), second.Code, second.Body.String(), second.Header().Get("Idempotency-Replayed"), calls.Load())
	}
	conflict := perform(engine, "order-123456", `"two"`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflict.Code)
	}
}

func TestConcurrentDuplicateRunsHandlerOnce(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	engine := engineFor(p, route, func(c *gin.Context) { calls.Add(1); close(entered); <-release; c.String(http.StatusOK, "done") })
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- perform(engine, "concurrent-key", `{}`) }()
	<-entered
	second := perform(engine, "concurrent-key", `{}`)
	if second.Code != http.StatusTooEarly {
		t.Fatalf("pending status = %d body=%s", second.Code, second.Body.String())
	}
	close(release)
	first := <-firstDone
	if first.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first=%d calls=%d", first.Code, calls.Load())
	}
}

func TestOnlyMarkedRoutesApplyAndFailuresRelease(t *testing.T) {
	p := initializedPlugin(t, func(c *Config) { c.MaxResponseBytes = 4 })
	unmarked := web.RouteInfo{Method: http.MethodPost, Path: "/orders"}
	if got := perform(engineFor(p, unmarked, func(c *gin.Context) { c.Status(http.StatusNoContent) }), "", "").Code; got != http.StatusNoContent {
		t.Fatalf("unmarked = %d", got)
	}
	marked := unmarked
	marked.Idempotent = true
	var calls atomic.Int32
	engine := engineFor(p, marked, func(c *gin.Context) {
		n := calls.Add(1)
		if n == 1 {
			c.String(http.StatusInternalServerError, "failed")
		} else {
			c.String(http.StatusOK, "response-too-large")
		}
	})
	if got := perform(engine, "retry-key", "").Code; got != http.StatusInternalServerError {
		t.Fatalf("failed = %d", got)
	}
	if got := perform(engine, "retry-key", "").Code; got != http.StatusOK {
		t.Fatalf("retry = %d", got)
	}
	if got := perform(engine, "retry-key", "").Code; got != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("oversize was cached: status=%d calls=%d", got, calls.Load())
	}
}

func TestKeyAndRequestBodyLimits(t *testing.T) {
	p := initializedPlugin(t, func(c *Config) { c.MaxRequestBytes = 2 })
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	if got := perform(engine, "", "").Code; got != http.StatusBadRequest {
		t.Fatalf("missing key = %d", got)
	}
	if got := perform(engine, "valid-key", "abc").Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body = %d", got)
	}
}

func TestQueryAndPrincipalScopeArePartOfSemantics(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(c *gin.Context) { calls.Add(1); c.Status(http.StatusNoContent) })
	if got := performPath(engine, "/orders?mode=fast", "semantic-key", `{}`).Code; got != http.StatusNoContent {
		t.Fatalf("first = %d", got)
	}
	if got := performPath(engine, "/orders?mode=slow", "semantic-key", `{}`).Code; got != http.StatusConflict {
		t.Fatalf("query conflict = %d", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestDuplicateKeyHeaderAndInvalidInjectedReplayAreRejected(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.Header.Add("Idempotency-Key", "duplicate-one")
	req.Header.Add("Idempotency-Key", "duplicate-two")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d", recorder.Code)
	}

	bad := initializedPlugin(t, func(c *Config) { c.MaxResponseBytes = 4 },
		WithStore(completedStore{response: Response{Status: 200, Body: make([]byte, 32)}}))
	response := perform(engineFor(bad, route, func(c *gin.Context) { c.Status(204) }), "invalid-store", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid replay status = %d", response.Code)
	}
}

type completedStore struct{ response Response }

func (s completedStore) Acquire(context.Context, string, string, string, time.Duration) (AcquireResult, error) {
	return AcquireResult{State: Completed, Response: s.response}, nil
}
func (completedStore) Complete(context.Context, string, string, string, Response, time.Duration) error {
	return nil
}
func (completedStore) Release(context.Context, string, string, string) error { return nil }
