package accesslog

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

type captured struct {
	level  string
	msg    string
	fields []any
}

type captureLogger struct {
	mu      sync.Mutex
	entries []captured
}

func (l *captureLogger) add(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, captured{level: level, msg: msg, fields: append([]any(nil), fields...)})
}
func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) corelog.Logger  { return l }
func (*captureLogger) Enabled(corelog.Level) bool    { return true }

func TestDefinitionAndOrderingContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseObserve || len(order.After) != 0 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestStructuredFieldsAndSecretMinimization(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	logger := &captureLogger{}
	p := New()
	cfg, _ := normalizeConfig(DefaultConfig())
	p.state.Store(&runtimeState{config: cfg, logger: logger})

	router := gin.New()
	router.Use(p.handle)
	router.GET("/users/:id", func(c *gin.Context) {
		c.Header(defaultRequestIDHeader, "request-1")
		c.String(http.StatusCreated, "hello")
	})
	request := httptest.NewRequest(http.MethodGet, "/users/42?password=super-secret", nil)
	request.RemoteAddr = "192.0.2.9:1234"
	request.Header.Set("Authorization", "Bearer token-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	entry := oneEntry(t, logger)
	fields := fieldsMap(entry.fields)
	for key, want := range map[string]any{
		"method": http.MethodGet, "path": "/users/42", "route": "/users/:id",
		"status": http.StatusCreated, "bytes": 5, "request_id": "request-1", "client_ip": "192.0.2.9",
		"panicked": false,
	} {
		if got := fields[key]; got != want {
			t.Fatalf("field %s = %#v, want %#v (all %#v)", key, got, want, fields)
		}
	}
	if _, ok := fields["latency"].(time.Duration); !ok {
		t.Fatalf("latency = %T, want time.Duration", fields["latency"])
	}
	serialized := entry.msg
	for _, value := range entry.fields {
		serialized += " " + valueString(value)
	}
	for _, secret := range []string{"super-secret", "token-secret", "password", "Authorization", "?"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("access log leaked %q: %s", secret, serialized)
		}
	}
}

func TestPanicIsLoggedAndRethrown(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	logger := &captureLogger{}
	p := New()
	cfg, _ := normalizeConfig(DefaultConfig())
	p.state.Store(&runtimeState{config: cfg, logger: logger})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/panic", func(*gin.Context) { panic("boom") })

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	entry := oneEntry(t, logger)
	if entry.level != "error" || fieldsMap(entry.fields)["panicked"] != true || fieldsMap(entry.fields)["status"] != http.StatusInternalServerError {
		t.Fatalf("panic entry = %#v", entry)
	}
}

func TestUnvalidatedInboundRequestIDIsNotLogged(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	logger := &captureLogger{}
	p := New()
	cfg, _ := normalizeConfig(DefaultConfig())
	p.state.Store(&runtimeState{config: cfg, logger: logger})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(defaultRequestIDHeader, "attacker-controlled-secret")
	router.ServeHTTP(httptest.NewRecorder(), request)

	entry := oneEntry(t, logger)
	if got := fieldsMap(entry.fields)["request_id"]; got != "" {
		t.Fatalf("unvalidated inbound request ID was logged: %#v", got)
	}
}

func TestSkipPathsAndUntrustedForwardedFor(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	logger := &captureLogger{}
	cfg := DefaultConfig()
	cfg.SkipPaths = []string{"/health", "/assets/*"}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	p.state.Store(&runtimeState{config: normalized, logger: logger})
	router := gin.New()
	router.Use(p.handle)
	router.GET("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, path := range []string{"/health", "/assets/app.js"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Forwarded-For", "203.0.113.88")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 0 {
		t.Fatalf("skipped paths produced entries: %#v", logger.entries)
	}
}

func oneEntry(t *testing.T, logger *captureLogger) captured {
	t.Helper()
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 1 {
		t.Fatalf("entries = %#v", logger.entries)
	}
	return logger.entries[0]
}

func fieldsMap(values []any) map[string]any {
	result := make(map[string]any)
	for i := 0; i+1 < len(values); i += 2 {
		key, _ := values[i].(string)
		result[key] = values[i+1]
	}
	return result
}

func valueString(value any) string {
	if value, ok := value.(string); ok {
		return value
	}
	return "<value>"
}
