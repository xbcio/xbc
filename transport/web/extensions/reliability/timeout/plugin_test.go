package timeout

import (
	"encoding/json"
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
