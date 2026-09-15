package recovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

type captureLogger struct {
	mu      sync.Mutex
	entries []captureEntry
}

type captureEntry struct {
	level  string
	msg    string
	fields []any
}

func (l *captureLogger) add(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, captureEntry{level: level, msg: msg, fields: append([]any(nil), fields...)})
}
func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) corelog.Logger  { return l }
func (*captureLogger) Enabled(corelog.Level) bool    { return true }

func TestDefinitionAndMiddlewareContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseRecover || len(order.Before) != 0 || len(order.After) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestRecoversWithSafe500AndSafeLog(t *testing.T) {
	logger := &captureLogger{}
	p := New()
	p.state.Store(&runtimeState{stack: false, logger: logger})

	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/panic", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("Content-Encoding", "br")
		c.SetHeader("Content-Length", "999")
		c.SetHeader("Content-Type", "text/plain")
		panic("secret-token-value")
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic?password=also-secret", nil)
	request.Header.Set("Authorization", "Bearer very-secret")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != http.StatusInternalServerError || problem.Properties["code"] != "internal_server_error" || problem.Instance != "/panic" {
		t.Fatalf("problem = %#v", problem)
	}
	if response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("unsafe panic response headers = %#v", response.Header())
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 1 || logger.entries[0].level != "error" {
		t.Fatalf("entries = %#v", logger.entries)
	}
	serialized := logger.entries[0].msg
	for _, field := range logger.entries[0].fields {
		serialized += " " + stringify(field)
	}
	for _, secret := range []string{"secret-token-value", "also-secret", "very-secret", "Authorization", "password"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("recovery log leaked %q: %s", secret, serialized)
		}
	}
}

func TestDoesNotOverwriteCommittedResponse(t *testing.T) {
	p := New()
	p.state.Store(&runtimeState{logger: corelog.Nop()})
	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/partial", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusAccepted, "already written")
		panic("boom")
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/partial", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "already written" {
		t.Fatalf("committed response changed: %d %q", response.Code, response.Body.String())
	}
}

// TestBrokenConnectionRecoveryDoesNotReachTheErrorBoundary pins the D2 fix:
// once recovery has itself logged and aborted a broken-connection panic, the
// error boundary must see no error at all -- not merely render harmlessly.
// Before this fix, recovery also reported the recovered error onward, so the
// outer web.OnError boundary would map it after the aborted chain unwound, log
// a second, redundant 500-level entry, and attempt to write a Problem Detail
// response onto a connection recovery had already given up on. Written()==false
// at that point (Abort does not write), so this double-handling was silent in
// production and only visible by inspecting the log and response together --
// exactly what this test asserts. Any regression that reports the recovered
// error again makes the injected mapper observe it and makes the recorder pick
// up a written response, so either assertion below would fail without needing
// to reach into the error boundary's own logger.
func TestBrokenConnectionRecoveryDoesNotReachTheErrorBoundary(t *testing.T) {
	logger := &captureLogger{}
	p := New()
	p.state.Store(&runtimeState{logger: logger})

	mapperCalled := false
	mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		mapperCalled = true
		return web.ProblemDetail{}, false
	})

	router := enginetest.New()
	router.Use(web.OnError(mapper))
	router.Use(p.handle)
	router.GET("/broken", func(context.Context, *web.Ctx) error {
		panic(syscall.EPIPE)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/broken", nil))

	if mapperCalled {
		t.Fatal("error boundary must never see the broken-connection panic recovery already handled")
	}
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("error boundary must not write to a connection recovery already aborted: code=%d body=%q",
			response.Code, response.Body.String())
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 1 || logger.entries[0].level != "warn" {
		t.Fatalf("expected exactly one Warn entry from recovery itself, got %#v", logger.entries)
	}
}

func stringify(value any) string {
	switch value := value.(type) {
	case string:
		return value
	default:
		return "<value>"
	}
}
