package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

func serveSessionRequest(p *Plugin, route web.RouteInfo, cookies []*http.Cookie, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, route)
		c.Next()
	})
	engine.Use(p.Handler())
	engine.Handle(route.Method, route.Path, handler)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(route.Method, route.Path, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	engine.ServeHTTP(recorder, request)
	return recorder
}

func initializedSessionPlugin(t *testing.T, store Store, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

func TestMiddlewareAuthenticatesAndPublishesDefensiveSessionPrincipal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	id := testID(7, 32)
	if err := store.Create(context.Background(), sessionValue(id, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	p := initializedSessionPlugin(t, store, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	response := serveSessionRequest(p, route, []*http.Cookie{{Name: defaultCookieName, Value: id}}, func(c *gin.Context) {
		principal, ok := web.CurrentPrincipal(c)
		if !ok || principal.Subject != "alice" || principal.AuthMethod != "session" || principal.Attributes["role"] != "admin" {
			c.Status(http.StatusInternalServerError)
			return
		}
		current, ok := Current(c)
		if !ok || current.ID != id || current.Subject != "alice" {
			c.Status(http.StatusInternalServerError)
			return
		}
		principal.Attributes["nested"].(map[string]any)["team"] = "principal-mutated"
		again, _ := Current(c)
		if again.Attributes["nested"].(map[string]any)["team"] != "core" {
			c.Status(http.StatusInternalServerError)
			return
		}
		current.Attributes["nested"].(map[string]any)["team"] = "current-mutated"
		again, _ = Current(c)
		if again.Attributes["nested"].(map[string]any)["team"] != "core" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMiddlewarePublicRouteBypassesAuthenticationAndBadCookie(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	public := web.Public()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &public}
	response := serveSessionRequest(p, route, []*http.Cookie{{Name: defaultCookieName, Value: "malformed"}}, func(c *gin.Context) {
		if _, ok := web.CurrentPrincipal(c); ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMiddlewareRejectsMissingMalformedDuplicateUnknownAndExpiredCookies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	for name, cookies := range map[string][]*http.Cookie{
		"missing":   nil,
		"malformed": {{Name: defaultCookieName, Value: "not-an-opaque-id"}},
		"unknown":   {{Name: defaultCookieName, Value: testID(20, 32)}},
		"duplicate": {{Name: defaultCookieName, Value: testID(21, 32)}, {Name: defaultCookieName, Value: testID(22, 32)}},
	} {
		t.Run(name, func(t *testing.T) {
			localNow := now
			store := newControlledMemoryStore(t, &localNow)
			p := initializedSessionPlugin(t, store, nil)
			response := serveSessionRequest(p, route, cookies, func(c *gin.Context) { c.Status(http.StatusNoContent) })
			assertUnauthorizedSession(t, response)
			if len(response.Result().Cookies()) == 0 {
				t.Fatal("rejected credential did not clear the session cookie")
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		localNow := now
		store := newControlledMemoryStore(t, &localNow)
		id := testID(23, 32)
		if err := store.Create(context.Background(), sessionValue(id, "alice", localNow, time.Minute), time.Minute); err != nil {
			t.Fatal(err)
		}
		localNow = localNow.Add(2 * time.Minute)
		p := initializedSessionPlugin(t, store, nil)
		response := serveSessionRequest(p, route, []*http.Cookie{{Name: defaultCookieName, Value: id}}, func(c *gin.Context) { c.Status(http.StatusNoContent) })
		assertUnauthorizedSession(t, response)
	})
}

func TestMiddlewareBoundsStoreOperationAndHidesBackendErrors(t *testing.T) {
	store := &recordingStore{touch: func(ctx context.Context, _ string, _, _ time.Duration) (Session, bool, error) {
		<-ctx.Done()
		return Session{}, false, ctx.Err()
	}}
	p := initializedSessionPlugin(t, store, func(cfg *Config) { cfg.OperationTimeout = time.Millisecond })
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	response := serveSessionRequest(p, route, []*http.Cookie{{Name: defaultCookieName, Value: testID(24, 32)}}, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	assertUnauthorizedSession(t, response)
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("backend errors should not mutate the client cookie")
	}

	errorStore := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
		return Session{}, false, errors.New("backend details")
	}}
	errorPlugin := initializedSessionPlugin(t, errorStore, nil)
	response = serveSessionRequest(errorPlugin, route, []*http.Cookie{{Name: defaultCookieName, Value: testID(25, 32)}}, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	assertUnauthorizedSession(t, response)
	if strings.Contains(response.Body.String(), "backend details") {
		t.Fatalf("backend details leaked: %q", response.Body.String())
	}
}

func assertUnauthorizedSession(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusUnauthorized || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("response=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != http.StatusUnauthorized || problem.Properties["code"] != "unauthorized" || problem.Instance != "/private" {
		t.Fatalf("problem=%#v", problem)
	}
}
