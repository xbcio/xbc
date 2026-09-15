package cors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func corsEngine(t *testing.T, cfg Config, downstream *int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(web.Handle(p.Handler()))
	engine.Any("/resource", func(c *gin.Context) {
		(*downstream)++
		c.Status(http.StatusOK)
	})
	return engine
}

func perform(engine http.Handler, method, origin string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/resource", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	return response
}

func TestActualRequestWritesConfiguredHeaders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://app.example"}
	cfg.AllowCredentials = true
	cfg.ExposeHeaders = []string{"X-Request-ID", "ETag"}
	downstream := 0
	response := perform(corsEngine(t, cfg, &downstream), http.MethodGet, "https://app.example", nil)

	if response.Code != http.StatusOK || downstream != 1 {
		t.Fatalf("status/downstream = %d/%d, want 200/1", response.Code, downstream)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Allow-Credentials = %q", got)
	}
	if got := response.Header().Get("Access-Control-Expose-Headers"); got != "X-Request-Id, Etag" {
		t.Fatalf("Expose-Headers = %q", got)
	}
	if !slices.Contains(response.Header().Values("Vary"), "Origin") {
		t.Fatalf("Vary = %v, want Origin", response.Header().Values("Vary"))
	}
}

func TestWildcardOriginNeverReflectsRequestOrigin(t *testing.T) {
	downstream := 0
	response := perform(corsEngine(t, DefaultConfig(), &downstream), http.MethodGet, "https://arbitrary.example", nil)
	if response.Code != http.StatusOK || downstream != 1 {
		t.Fatalf("status/downstream = %d/%d, want 200/1", response.Code, downstream)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Allow-Credentials = %q, want empty", got)
	}
}

func TestDisallowedOriginIsRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://allowed.example"}
	downstream := 0
	response := perform(corsEngine(t, cfg, &downstream), http.MethodGet, "https://denied.example", nil)
	if response.Code != http.StatusForbidden || downstream != 0 {
		t.Fatalf("status/downstream = %d/%d, want 403/0", response.Code, downstream)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty", got)
	}
}

func TestPreflightValidatesMethodAndHeaders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://app.example"}
	cfg.AllowMethods = []string{"GET", "POST"}
	cfg.AllowHeaders = []string{"Content-Type", "X-Trace-ID"}
	cfg.MaxAge = 90 * time.Second

	t.Run("allowed", func(t *testing.T) {
		downstream := 0
		response := perform(corsEngine(t, cfg, &downstream), http.MethodOptions, "https://app.example", map[string]string{
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "content-type, x-trace-id",
		})
		if response.Code != http.StatusNoContent || downstream != 0 {
			t.Fatalf("status/downstream = %d/%d, want 204/0", response.Code, downstream)
		}
		checks := map[string]string{
			"Access-Control-Allow-Origin":  "https://app.example",
			"Access-Control-Allow-Methods": "GET, POST",
			"Access-Control-Allow-Headers": "Content-Type, X-Trace-Id",
			"Access-Control-Max-Age":       "90",
		}
		for key, want := range checks {
			if got := response.Header().Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	})

	for _, tc := range []struct {
		name    string
		method  string
		headers string
	}{
		{"method", "DELETE", "content-type"},
		{"header", "POST", "x-not-allowed"},
		{"malformed header", "POST", "x-ok, bad header"},
	} {
		t.Run("denied "+tc.name, func(t *testing.T) {
			downstream := 0
			response := perform(corsEngine(t, cfg, &downstream), http.MethodOptions, "https://app.example", map[string]string{
				"Access-Control-Request-Method":  tc.method,
				"Access-Control-Request-Headers": tc.headers,
			})
			if response.Code != http.StatusForbidden || downstream != 0 {
				t.Fatalf("status/downstream = %d/%d, want 403/0", response.Code, downstream)
			}
		})
	}
}

func TestRequestWithoutOriginPassesThrough(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://allowed.example"}
	downstream := 0
	response := perform(corsEngine(t, cfg, &downstream), http.MethodGet, "", nil)
	if response.Code != http.StatusOK || downstream != 1 {
		t.Fatalf("status/downstream = %d/%d, want 200/1", response.Code, downstream)
	}
}

func TestWildcardMethodsAndHeadersReflectValidatedPreflightValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowMethods = []string{"*"}
	cfg.AllowHeaders = []string{"*"}
	downstream := 0
	response := perform(corsEngine(t, cfg, &downstream), http.MethodOptions, "https://app.example", map[string]string{
		"Access-Control-Request-Method":  "PROPFIND",
		"Access-Control-Request-Headers": "x-custom, authorization",
	})
	if response.Code != http.StatusNoContent || downstream != 0 {
		t.Fatalf("status/downstream = %d/%d, want 204/0", response.Code, downstream)
	}
	if got := response.Header().Get("Access-Control-Allow-Methods"); got != "PROPFIND" {
		t.Fatalf("Allow-Methods = %q, want PROPFIND", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom, Authorization" {
		t.Fatalf("Allow-Headers = %q, want reflected headers", got)
	}
}

func TestPreflightMethodMatchingIsCaseSensitive(t *testing.T) {
	cfg := DefaultConfig()
	downstream := 0
	response := perform(corsEngine(t, cfg, &downstream), http.MethodOptions, "https://app.example", map[string]string{
		"Access-Control-Request-Method": "get",
	})
	if response.Code != http.StatusForbidden || downstream != 0 {
		t.Fatalf("status/downstream = %d/%d, want 403/0", response.Code, downstream)
	}
}

// TestPreflightAbortIsObservableAsWrittenByOuterMiddleware pins the two-step
// commit this middleware relies on: Ctx.Status only records a pending status,
// so a preflight abort must still call Write to flip the underlying writer's
// Written() to true before c.Abort() returns control to outer middleware. If
// that write were ever dropped, an outer error boundary or logging middleware
// resuming after c.Next() would see an unwritten response and could double-
// write it.
func TestPreflightAbortIsObservableAsWrittenByOuterMiddleware(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://allowed.example"}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	var seenWritten bool
	var seenStatus int
	engine := gin.New()
	engine.Use(web.Handle(func(_ context.Context, c *web.Ctx) error {
		c.Next()
		seenWritten = c.Writer().Written()
		seenStatus = c.Writer().Status()
		return nil
	}))
	engine.Use(web.Handle(p.Handler()))
	engine.OPTIONS("/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

	response := perform(engine, http.MethodOptions, "https://allowed.example", map[string]string{
		"Access-Control-Request-Method": http.MethodGet,
	})

	if !seenWritten {
		t.Fatal("outer middleware did not observe the preflight response as written")
	}
	if seenStatus != http.StatusNoContent {
		t.Fatalf("seenStatus = %d, want 204", seenStatus)
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
}
