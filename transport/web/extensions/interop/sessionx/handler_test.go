package sessionx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web/extensions/interop/sessionx"
)

var errStoreUnavailable = errors.New("test store unavailable")

// newTestCore builds the real neutral plugin on its in-process backend. Its
// defaults are tightened so the cookie attributes are stable and a test can
// override one thing at a time.
func newTestCore(t *testing.T, mutate func(*session.Config)) *session.Plugin {
	t.Helper()
	config := session.DefaultConfig()
	config.Name = "xbc_session"
	config.Path = "/"
	config.Domain = ""
	config.Secure = true
	config.SameSite = "lax"
	config.TTL = time.Hour
	config.IdleTTL = 30 * time.Minute
	config.TouchInterval = 0
	if mutate != nil {
		mutate(&config)
	}
	core, err := session.New(config)
	if err != nil {
		t.Fatalf("session.New() error = %v", err)
	}
	t.Cleanup(func() { _ = core.Stop(context.Background()) })
	return core
}

// newTestCoreWithStore builds the neutral plugin over an explicit store, for
// tests that need to inject a failure or observe a call.
func newTestCoreWithStore(t *testing.T, store session.Store) *session.Plugin {
	t.Helper()
	config := session.DefaultConfig()
	config.Name = "xbc_session"
	config.TTL = time.Hour
	config.IdleTTL = 30 * time.Minute
	config.TouchInterval = 0
	core, err := session.New(config, session.WithStore(store))
	if err != nil {
		t.Fatalf("session.New(WithStore) error = %v", err)
	}
	t.Cleanup(func() { _ = core.Stop(context.Background()) })
	return core
}

func newMemoryStore(t *testing.T) *session.MemoryStore {
	t.Helper()
	store, err := session.NewMemoryStore(time.Hour)
	if err != nil {
		t.Fatalf("session.NewMemoryStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestHandler(t *testing.T, core *session.Plugin, opts ...sessionx.Option) *sessionx.Handler {
	t.Helper()
	handler, err := sessionx.New(core, core.Cookie(), core.Lifetime, opts...)
	if err != nil {
		t.Fatalf("sessionx.New() error = %v", err)
	}
	return handler
}

func newTestContext(cookies ...*http.Cookie) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	ctx.Request = request
	return ctx, recorder
}

func createSession(t *testing.T, core *session.Plugin, subject string, attributes map[string]any) session.Session {
	t.Helper()
	value, err := core.Manager().Create(context.Background(), subject, attributes)
	if err != nil {
		t.Fatalf("Create(%q) error = %v", subject, err)
	}
	return value
}

func cookieFor(core *session.Plugin, value session.Session) *http.Cookie {
	return &http.Cookie{Name: core.Cookie().Name, Value: value.ID}
}

func TestNewValidatesItsInputs(t *testing.T) {
	core := newTestCore(t, nil)
	cookie := core.Cookie()

	t.Run("nil manager", func(t *testing.T) {
		if _, err := sessionx.New(nil, cookie, core.Lifetime); err == nil {
			t.Fatal("New() accepted a nil manager")
		}
	})

	t.Run("nil lifetime", func(t *testing.T) {
		if _, err := sessionx.New(core, cookie, nil); err == nil {
			t.Fatal("New() accepted a nil lifetime function")
		}
	})

	t.Run("script-readable cookie", func(t *testing.T) {
		unsafe := cookie
		unsafe.HTTPOnly = false
		if _, err := sessionx.New(core, unsafe, core.Lifetime); err == nil {
			t.Fatal("New() accepted http_only=false")
		}
	})

	t.Run("SameSite=None without Secure", func(t *testing.T) {
		unsafe := cookie
		unsafe.Secure = false
		unsafe.SameSite = http.SameSiteNoneMode
		if _, err := sessionx.New(core, unsafe, core.Lifetime); err == nil {
			t.Fatal("New() accepted SameSite=None without secure")
		}
	})

	t.Run("blank context key", func(t *testing.T) {
		if _, err := sessionx.New(core, cookie, core.Lifetime, sessionx.WithContextKey("  ")); err == nil {
			t.Fatal("New() accepted a blank context key")
		}
	})

	t.Run("valid", func(t *testing.T) {
		handler, err := sessionx.New(core, cookie, core.Lifetime, sessionx.WithContextKey("custom"))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if handler == nil {
			t.Fatal("New() = nil, nil")
		}
	})
}

func TestMiddlewarePublishesTheRequestOutcome(t *testing.T) {
	t.Run("valid session", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", map[string]any{"role": "admin"})
		ctx, _ := newTestContext(cookieFor(core, value))

		handler.Middleware()(ctx)

		got, ok := handler.Current(ctx)
		if !ok || got.ID != value.ID || got.Subject != "alice" {
			t.Fatalf("Current() = %+v, %v; want the created session", got, ok)
		}
		if got.Attributes["role"] != "admin" {
			t.Fatalf("Attributes = %+v, want role=admin", got.Attributes)
		}
		if id, ok := handler.CurrentID(ctx); !ok || id != value.ID {
			t.Fatalf("CurrentID() = %q, %v; want %q, true", id, ok, value.ID)
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil", err)
		}

		// The package-level helpers read the default key, which a Handler
		// without WithContextKey uses.
		if got, ok := sessionx.Current(ctx); !ok || got.ID != value.ID {
			t.Fatalf("package Current() = %+v, %v; want the created session", got, ok)
		}
		if id, ok := sessionx.CurrentID(ctx); !ok || id != value.ID {
			t.Fatalf("package CurrentID() = %q, %v; want %q, true", id, ok, value.ID)
		}
		if !sessionx.Presented(ctx) {
			t.Fatal("package Presented() = false, want true")
		}
		if err := sessionx.StoreError(ctx); err != nil {
			t.Fatalf("package StoreError() = %v, want nil", err)
		}
	})

	t.Run("no cookie", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		ctx, _ := newTestContext()

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() reported a session without a cookie")
		}
		if _, ok := handler.CurrentID(ctx); ok {
			t.Fatal("CurrentID() reported a session without a cookie")
		}
		if handler.Presented(ctx) {
			t.Fatal("Presented() = true without a cookie")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil", err)
		}
	})

	t.Run("malformed ID", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		ctx, _ := newTestContext(&http.Cookie{Name: core.Cookie().Name, Value: "not-an-opaque-id"})

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() reported a session for a malformed ID")
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil: a malformed ID is a credential problem", err)
		}
	})

	t.Run("expired session", func(t *testing.T) {
		core := newTestCore(t, func(config *session.Config) {
			config.TTL = 20 * time.Millisecond
			config.IdleTTL = 20 * time.Millisecond
			config.TouchInterval = time.Millisecond
		})
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", nil)
		time.Sleep(60 * time.Millisecond)
		ctx, _ := newTestContext(cookieFor(core, value))

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() reported an expired session")
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil: expiry is not a storage failure", err)
		}
	})

	t.Run("duplicated cookie", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", nil)
		ctx, _ := newTestContext(
			cookieFor(core, value),
			&http.Cookie{Name: core.Cookie().Name, Value: "another-value"},
		)

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() picked one of two presented cookies")
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil: duplication is a credential problem", err)
		}
	})

	t.Run("empty cookie", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		ctx, _ := newTestContext(&http.Cookie{Name: core.Cookie().Name, Value: ""})

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() verified an empty cookie")
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true: the client sent the cookie")
		}
		if err := handler.StoreError(ctx); err != nil {
			t.Fatalf("StoreError() = %v, want nil", err)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		store := &failingStore{MemoryStore: newMemoryStore(t), err: errStoreUnavailable}
		core := newTestCoreWithStore(t, store)
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", nil)
		ctx, _ := newTestContext(cookieFor(core, value))

		handler.Middleware()(ctx)

		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() reported a session after a storage failure")
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true")
		}
		if err := handler.StoreError(ctx); !errors.Is(err, errStoreUnavailable) {
			t.Fatalf("StoreError() = %v, want %v", err, errStoreUnavailable)
		}
		if !errors.Is(sessionx.StoreError(ctx), errStoreUnavailable) {
			t.Fatal("package StoreError() did not expose the storage failure")
		}
	})
}

func TestMiddlewareDoesNotAbortOrWrite(t *testing.T) {
	core := newTestCore(t, nil)
	handler := newTestHandler(t, core)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(handler.Middleware())

	reached := false
	engine.GET("/", func(c *gin.Context) {
		reached = true
		if c.IsAborted() {
			t.Error("middleware aborted the chain")
		}
		if _, ok := handler.Current(c); ok {
			t.Error("middleware published a session for a request without a cookie")
		}
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if !reached {
		t.Fatal("handler did not run; middleware stopped the chain")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: middleware wrote a response", recorder.Code, http.StatusNoContent)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("middleware set cookies: %#v", cookies)
	}
}

func TestLogin(t *testing.T) {
	t.Run("writes the configured cookie and publishes the session", func(t *testing.T) {
		core := newTestCore(t, func(config *session.Config) {
			config.Name = "sid"
			config.Path = "/app"
			config.Domain = "example.com"
			config.SameSite = "strict"
		})
		handler := newTestHandler(t, core)
		ctx, recorder := newTestContext()

		value, err := handler.Login(ctx, "alice", map[string]any{"role": "admin"})
		if err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		if value.Subject != "alice" || value.Attributes["role"] != "admin" {
			t.Fatalf("session = %+v, want subject alice with role admin", value)
		}
		if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != session.VerdictValid {
			t.Fatalf("Verify(new session) = %v, %v; want valid", verdict, err)
		}

		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("cookies = %#v, want one session cookie", cookies)
		}
		cookie := cookies[0]
		if cookie.Name != "sid" || cookie.Value != value.ID {
			t.Fatalf("cookie = %#v, want name sid and value of the new session", cookie)
		}
		if cookie.Path != "/app" || cookie.Domain != "example.com" {
			t.Fatalf("cookie Path/Domain = %q/%q, want /app/example.com", cookie.Path, cookie.Domain)
		}
		if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("cookie security attributes = %#v", cookie)
		}
		if cookie.Expires.Unix() != value.ExpiresAt.Unix() {
			t.Fatalf("cookie Expires = %v, want %v", cookie.Expires, value.ExpiresAt)
		}
		if cookie.MaxAge <= 3500 || cookie.MaxAge > 3600 {
			t.Fatalf("cookie MaxAge = %d, want the one-hour lifetime rounded up to seconds", cookie.MaxAge)
		}

		if got, ok := handler.Current(ctx); !ok || got.ID != value.ID {
			t.Fatalf("Current() after Login() = %+v, %v; want the new session", got, ok)
		}
		if handler.Presented(ctx) {
			t.Fatal("Presented() = true after Login(), but the request carried no cookie")
		}
	})

	t.Run("revokes a session the request already presents", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		previous := createSession(t, core, "alice", nil)
		ctx, recorder := newTestContext(cookieFor(core, previous))

		value, err := handler.Login(ctx, "alice", nil)
		if err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		if value.ID == previous.ID {
			t.Fatal("Login() reused the presented session ID")
		}
		if _, verdict, err := core.Verify(context.Background(), previous.ID); err != nil || verdict != session.VerdictNotFound {
			t.Fatalf("Verify(old session) = %v, %v; want not found", verdict, err)
		}
		if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != session.VerdictValid {
			t.Fatalf("Verify(new session) = %v, %v; want valid", verdict, err)
		}
		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Value != value.ID {
			t.Fatalf("cookies = %#v, want the new session ID", cookies)
		}
		if !handler.Presented(ctx) {
			t.Fatal("Presented() = false, want true: the request did carry a cookie")
		}
	})

	t.Run("rejects a blank subject without creating or revoking anything", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		previous := createSession(t, core, "alice", nil)
		ctx, recorder := newTestContext(cookieFor(core, previous))

		if _, err := handler.Login(ctx, "   ", nil); !errors.Is(err, sessionx.ErrSubjectRequired) {
			t.Fatalf("Login(blank) = %v, want ErrSubjectRequired", err)
		}
		if len(recorder.Result().Cookies()) != 0 {
			t.Fatalf("rejected login wrote cookies: %#v", recorder.Result().Cookies())
		}
		if _, verdict, err := core.Verify(context.Background(), previous.ID); err != nil || verdict != session.VerdictValid {
			t.Fatalf("Verify(presented session) = %v, %v; want it untouched", verdict, err)
		}
		sessions, err := core.Manager().ListBySubject(context.Background(), "alice")
		if err != nil {
			t.Fatalf("ListBySubject() error = %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("sessions = %+v, want the presented session alone", sessions)
		}
	})

	t.Run("revokes the new session when the cookie cannot be issued", func(t *testing.T) {
		core := newTestCore(t, nil)
		lifetimeErr := errors.New("lifetime unavailable")
		handler, err := sessionx.New(core, core.Cookie(), func(session.Session) (time.Duration, error) {
			return 0, lifetimeErr
		})
		if err != nil {
			t.Fatalf("sessionx.New() error = %v", err)
		}
		ctx, recorder := newTestContext()

		if _, err := handler.Login(ctx, "alice", nil); !errors.Is(err, lifetimeErr) {
			t.Fatalf("Login() = %v, want the lifetime error", err)
		}
		if len(recorder.Result().Cookies()) != 0 {
			t.Fatalf("failed login wrote cookies: %#v", recorder.Result().Cookies())
		}
		sessions, err := core.Manager().ListBySubject(context.Background(), "alice")
		if err != nil {
			t.Fatalf("ListBySubject() error = %v", err)
		}
		if len(sessions) != 0 {
			t.Fatalf("sessions = %+v, want the failed login rolled back", sessions)
		}
	})
}

func TestLogout(t *testing.T) {
	t.Run("revokes the session and clears the cookie", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", nil)
		ctx, recorder := newTestContext(cookieFor(core, value))
		handler.Middleware()(ctx)

		if err := handler.Logout(ctx); err != nil {
			t.Fatalf("Logout() error = %v", err)
		}
		if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != session.VerdictNotFound {
			t.Fatalf("Verify(logged-out session) = %v, %v; want not found", verdict, err)
		}

		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("cookies = %#v, want one clearing cookie", cookies)
		}
		cookie := cookies[0]
		if cookie.Name != core.Cookie().Name || cookie.Value != "" || cookie.MaxAge != -1 {
			t.Fatalf("clearing cookie = %#v", cookie)
		}
		if cookie.Expires.Unix() != 1 {
			t.Fatalf("clearing cookie Expires = %v, want the epoch sentinel", cookie.Expires)
		}
		if _, ok := handler.Current(ctx); ok {
			t.Fatal("Current() still reports the logged-out session")
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		ctx, recorder := newTestContext()

		if err := handler.Logout(ctx); err != nil {
			t.Fatalf("first Logout() error = %v", err)
		}
		if err := handler.Logout(ctx); err != nil {
			t.Fatalf("second Logout() error = %v", err)
		}
		cookies := recorder.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatalf("cookies = %#v, want one clearing cookie per call", cookies)
		}
		for _, cookie := range cookies {
			if cookie.MaxAge != -1 {
				t.Fatalf("clearing cookie = %#v, want MaxAge -1", cookie)
			}
		}
	})

	t.Run("treats a malformed presented cookie as already gone", func(t *testing.T) {
		core := newTestCore(t, nil)
		handler := newTestHandler(t, core)
		ctx, recorder := newTestContext(&http.Cookie{Name: core.Cookie().Name, Value: "not-an-opaque-id"})

		if err := handler.Logout(ctx); err != nil {
			t.Fatalf("Logout() error = %v", err)
		}
		if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
			t.Fatalf("cookies = %#v, want one clearing cookie", cookies)
		}
	})

	t.Run("clears the cookie even when revocation fails", func(t *testing.T) {
		store := &deleteFailingStore{MemoryStore: newMemoryStore(t), err: errStoreUnavailable}
		core := newTestCoreWithStore(t, store)
		handler := newTestHandler(t, core)
		value := createSession(t, core, "alice", nil)
		ctx, recorder := newTestContext(cookieFor(core, value))

		if err := handler.Logout(ctx); !errors.Is(err, errStoreUnavailable) {
			t.Fatalf("Logout() = %v, want the store error", err)
		}
		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 || cookies[0].MaxAge != -1 {
			t.Fatalf("cookies = %#v, want a clearing cookie despite the store failure", cookies)
		}
	})
}

func TestVerifyRenewsTheIdleLease(t *testing.T) {
	store := &recordingStore{MemoryStore: newMemoryStore(t)}
	core := newTestCoreWithStore(t, store)
	handler := newTestHandler(t, core)
	value := createSession(t, core, "alice", nil)
	ctx, _ := newTestContext(cookieFor(core, value))

	handler.Middleware()(ctx)

	if got := store.touchCount(); got != 1 {
		t.Fatalf("Store.Touch calls = %d, want 1: verification must reach the idle-lease renewal", got)
	}
	if _, ok := handler.Current(ctx); !ok {
		t.Fatal("Current() = false after a valid verification")
	}
}

func TestHandlersWithDifferentContextKeysDoNotShareOutcomes(t *testing.T) {
	firstCore := newTestCore(t, func(config *session.Config) { config.Name = "sid_first" })
	secondCore := newTestCore(t, func(config *session.Config) { config.Name = "sid_second" })
	first := newTestHandler(t, firstCore, sessionx.WithContextKey("first"))
	second := newTestHandler(t, secondCore, sessionx.WithContextKey("second"))
	value := createSession(t, firstCore, "alice", nil)
	ctx, _ := newTestContext(cookieFor(firstCore, value))

	first.Middleware()(ctx)
	second.Middleware()(ctx)

	if got, ok := first.Current(ctx); !ok || got.ID != value.ID {
		t.Fatalf("first Current() = %+v, %v; want the created session", got, ok)
	}
	if _, ok := second.Current(ctx); ok {
		t.Fatal("second Handler read the first Handler's session")
	}
	if second.Presented(ctx) {
		t.Fatal("second Handler reported a cookie that was never sent under its name")
	}
	if _, ok := sessionx.Current(ctx); ok {
		t.Fatal("package Current() read a custom-key outcome")
	}
	if _, exists := ctx.Get("first"); !exists {
		t.Fatal("first Handler published nothing under its own key")
	}
	if _, exists := ctx.Get("second"); !exists {
		t.Fatal("second Handler published nothing under its own key")
	}
}

// failingStore delegates storage to the real in-process store but makes every
// verification fail, standing in for an unavailable backend.
type failingStore struct {
	*session.MemoryStore
	err error
}

func (s *failingStore) Touch(context.Context, string, time.Duration, time.Duration) (session.Session, bool, error) {
	return session.Session{}, false, s.err
}

// deleteFailingStore delegates storage to the real in-process store but fails
// revocation.
type deleteFailingStore struct {
	*session.MemoryStore
	err error
}

func (s *deleteFailingStore) Delete(context.Context, string) error { return s.err }

// recordingStore counts the idle-lease renewals verification performs.
type recordingStore struct {
	*session.MemoryStore
	mu      sync.Mutex
	touches int
}

func (s *recordingStore) Touch(ctx context.Context, id string, idleTTL, minInterval time.Duration) (session.Session, bool, error) {
	s.mu.Lock()
	s.touches++
	s.mu.Unlock()
	return s.MemoryStore.Touch(ctx, id, idleTTL, minInterval)
}

func (s *recordingStore) touchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.touches
}
