package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const testSessionCookie = "xbc_session"

// --- fakes -----------------------------------------------------------------

type stubResolver struct {
	resolution federationcore.Resolution
	err        error
	resolved   []federationcore.ExternalIdentity
}

func (r *stubResolver) Resolve(_ context.Context, identity federationcore.ExternalIdentity) (federationcore.Resolution, error) {
	r.resolved = append(r.resolved, identity)
	return r.resolution, r.err
}

type sessionOperation struct {
	op         string
	id         string
	subject    string
	attributes map[string]any
}

type fakeSessions struct {
	mu           sync.Mutex
	ops          []sessionOperation
	nextID       string
	revokeErrors map[string]error
	createErr    error
	setCookieErr error
}

func (s *fakeSessions) record(operation sessionOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, operation)
}

func (s *fakeSessions) operations() []sessionOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sessionOperation(nil), s.ops...)
}

func (s *fakeSessions) Create(_ context.Context, subject string, attributes map[string]any) (sessioncore.Session, error) {
	s.record(sessionOperation{op: "create", subject: subject, attributes: attributes})
	if s.createErr != nil {
		return sessioncore.Session{}, s.createErr
	}
	id := s.nextID
	if id == "" {
		id = "new-session-id"
	}
	now := time.Now().UTC()
	return sessioncore.Session{ID: id, Subject: subject, Attributes: attributes, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, nil
}

func (s *fakeSessions) Rotate(_ context.Context, id string) (sessioncore.Session, error) {
	s.record(sessionOperation{op: "rotate", id: id})
	return sessioncore.Session{}, errors.New("rotate must not be called")
}

func (s *fakeSessions) Revoke(_ context.Context, id string) error {
	s.record(sessionOperation{op: "revoke", id: id})
	return s.revokeErrors[id]
}

func (s *fakeSessions) ListBySubject(_ context.Context, subject string) ([]sessioncore.SessionInfo, error) {
	s.record(sessionOperation{op: "list", subject: subject})
	return nil, nil
}

func (s *fakeSessions) RevokeBySubject(_ context.Context, subject string) (int, error) {
	s.record(sessionOperation{op: "revoke-subject", subject: subject})
	return 0, nil
}

func (s *fakeSessions) RevokeByDigest(_ context.Context, subject, digest string) (bool, error) {
	s.record(sessionOperation{op: "revoke-digest", subject: subject, id: digest})
	return false, nil
}

func (s *fakeSessions) SetCookie(_ *web.Ctx, value sessioncore.Session) error {
	s.record(sessionOperation{op: "set-cookie", id: value.ID})
	return s.setCookieErr
}

func (s *fakeSessions) ClearCookie(*web.Ctx) {
	s.record(sessionOperation{op: "clear-cookie"})
}

type capturedEntry struct {
	level string
	msg   string
	kv    []any
}

type capturingLogger struct {
	mu      sync.Mutex
	entries []capturedEntry
}

func (l *capturingLogger) record(level, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, capturedEntry{level: level, msg: msg, kv: append([]any(nil), kv...)})
}

func (l *capturingLogger) Debug(msg string, kv ...any) { l.record("debug", msg, kv...) }
func (l *capturingLogger) Info(msg string, kv ...any)  { l.record("info", msg, kv...) }
func (l *capturingLogger) Warn(msg string, kv ...any)  { l.record("warn", msg, kv...) }
func (l *capturingLogger) Error(msg string, kv ...any) { l.record("error", msg, kv...) }
func (l *capturingLogger) Fatal(msg string, kv ...any) { l.record("fatal", msg, kv...) }
func (l *capturingLogger) With(...any) log.Logger      { return l }
func (l *capturingLogger) Enabled(log.Level) bool      { return true }

func (l *capturingLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var builder strings.Builder
	for _, entry := range l.entries {
		fmt.Fprintf(&builder, "%s %s %v\n", entry.level, entry.msg, entry.kv)
	}
	return builder.String()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("entropy source failed") }

var _ io.Reader = errReader{}

type countingStore struct {
	Store
	creates      int
	consumes     int
	reservations int
}

func (s *countingStore) Create(ctx context.Context, tx Transaction) error {
	s.creates++
	return s.Store.Create(ctx, tx)
}

func (s *countingStore) Consume(ctx context.Context, provider, state, binding string) (Transaction, bool, error) {
	s.consumes++
	return s.Store.Consume(ctx, provider, state, binding)
}

func (s *countingStore) SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error) {
	s.reservations++
	return s.Store.SetIfAbsent(ctx, namespace, key, ttl)
}

type failingStore struct{}

func (failingStore) Create(context.Context, Transaction) error {
	return fmt.Errorf("%w: down", ErrTransactionStore)
}
func (failingStore) Consume(context.Context, string, string, string) (Transaction, bool, error) {
	return Transaction{}, false, fmt.Errorf("%w: down", ErrTransactionStore)
}
func (failingStore) SetIfAbsent(context.Context, string, string, time.Duration) (bool, error) {
	return false, fmt.Errorf("%w: down", ErrTransactionStore)
}

// --- helpers ---------------------------------------------------------------

func newLoginTestPlugin(t *testing.T, sessions *fakeSessions, resolver federationcore.IdentityResolver, mutate func(*Config), opts ...Option) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Backend = BackendMemory // the unit tests run against one in-process store
	cfg.CookieSecure = false    // httptest requests are plain HTTP
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, resolver, sessions, testSessionCookie, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

func resolvedIdentity(t *testing.T) federationcore.ExternalIdentity {
	t.Helper()
	authTime, err := time.Parse(time.RFC3339, "2026-10-18T07:30:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return federationcore.ExternalIdentity{
		Protocol: "oidc",
		Provider: "corp",
		Issuer:   "https://idp.example.com",
		Subject:  "idp-subject-1",
		Claims:   map[string]any{"email": "alice@example.com"},
		AuthTime: authTime,
	}
}

func newLoginCtx(t *testing.T, cookies ...*http.Cookie) (*web.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/federation/callback", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	return enginetest.NewCtx(recorder, request), recorder
}

func loginCookie(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response set no %q cookie; Set-Cookie = %q", name, recorder.Header().Values("Set-Cookie"))
	return nil
}

func beginLogin(t *testing.T, p *Plugin, returnTo string) (Transaction, *http.Cookie) {
	t.Helper()
	ctx, recorder := newLoginCtx(t)
	transaction, err := p.Begin(context.Background(), ctx, BeginRequest{
		Provider:     testProvider,
		State:        testState,
		Nonce:        "nonce-value",
		CodeVerifier: "pkce-verifier-value",
		RequestID:    "saml-request-id",
		ReturnTo:     returnTo,
	})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	return transaction, loginCookie(t, recorder, "xbc_federation")
}

func failureCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	location := recorder.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("Location %q is unparsable: %v", location, err)
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		t.Fatalf("failure landing %q is not same-origin", location)
	}
	return parsed.Query().Get("error")
}

func operationsOf(t *testing.T, sessions *fakeSessions) []string {
	t.Helper()
	var got []string
	for _, operation := range sessions.operations() {
		got = append(got, operation.op)
	}
	return got
}

// --- Begin -----------------------------------------------------------------

func TestBeginStoresTheTransactionAndInstallsTheBindingCookie(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	before := time.Now().UTC()
	transaction, cookie := beginLogin(t, p, "/dashboard?tab=1")
	after := time.Now().UTC()

	if transaction.Provider != testProvider || transaction.State != testState ||
		transaction.Nonce != "nonce-value" || transaction.CodeVerifier != "pkce-verifier-value" ||
		transaction.RequestID != "saml-request-id" || transaction.ReturnTo != "/dashboard?tab=1" {
		t.Fatalf("Begin() transaction = %#v", transaction)
	}
	if transaction.Binding != BrowserBinding(cookie.Value) {
		t.Fatal("transaction binding is not the digest of the cookie secret")
	}
	if transaction.Binding == cookie.Value {
		t.Fatal("transaction stored the raw cookie secret")
	}
	if len(cookie.Value) != 43 {
		t.Fatalf("binding cookie value length = %d, want 43 raw-base64url characters", len(cookie.Value))
	}
	if !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("binding cookie = %#v", cookie)
	}
	if cookie.MaxAge != int(MaxTransactionTTL/time.Second) {
		t.Fatalf("binding cookie MaxAge = %d", cookie.MaxAge)
	}
	if !transaction.ExpiresAt.After(before.Add(MaxTransactionTTL-time.Second)) || transaction.ExpiresAt.After(after.Add(MaxTransactionTTL+time.Second)) {
		t.Fatalf("transaction expiry = %s, want about now+%s", transaction.ExpiresAt, MaxTransactionTTL)
	}
}

func TestBindingCookieCarriesTheConfiguredSecureAndDomainAttributes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendMemory
	cfg.CookieDomain = "example.com"
	p, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie)
	if err != nil {
		t.Fatal(err)
	}
	transaction, cookie := beginLogin(t, p, "/")
	if !cookie.Secure {
		t.Fatalf("binding cookie is not Secure with the default cookie_secure=true: %#v", cookie)
	}
	if cookie.Domain != "example.com" {
		t.Fatalf("binding cookie Domain = %q, want the configured domain", cookie.Domain)
	}

	ctx, recorder := newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if transaction.Binding != BrowserBinding(cookie.Value) {
		t.Fatal("binding cookie does not match the stored digest")
	}
	cleared := loginCookie(t, recorder, cookie.Name)
	if !cleared.Secure || cleared.Domain != "example.com" {
		t.Fatalf("clearing cookie lost the configured attributes: %#v", cleared)
	}
}

func TestBeginSanitizesTheRequestedLanding(t *testing.T) {
	for name, raw := range map[string]string{
		"scheme relative": "//evil.example/dashboard",
		"other origin":    "https://evil.example/dashboard",
		"javascript":      "javascript:alert(1)",
		"backslash":       `/a\b`,
		"control":         "/ok\r\nLocation: https://evil.example",
		"oversized":       "/" + strings.Repeat("a", MaxReturnToLength),
		"absent":          "",
	} {
		t.Run(name, func(t *testing.T) {
			p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
			transaction, _ := beginLogin(t, p, raw)
			if transaction.ReturnTo != "/" {
				t.Fatalf("ReturnTo = %q, want the configured fallback", transaction.ReturnTo)
			}
		})
	}
}

func TestBeginAcceptsAllowlistedAbsoluteLandings(t *testing.T) {
	mutate := func(cfg *Config) {
		cfg.AllowedReturnOrigins = []string{"https://app.example.com"}
		cfg.DefaultReturnTo = "/home"
	}
	transaction, _ := beginLogin(t, newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, mutate), "https://app.example.com/dashboard")
	if transaction.ReturnTo != "https://app.example.com/dashboard" {
		t.Fatalf("ReturnTo = %q", transaction.ReturnTo)
	}
	transaction, _ = beginLogin(t, newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, mutate), "https://evil.example/dashboard")
	if transaction.ReturnTo != "/home" {
		t.Fatalf("ReturnTo = %q, want the configured fallback", transaction.ReturnTo)
	}
}

func TestBeginDoesNotInstallACookieWhenTheStoreFails(t *testing.T) {
	p, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(failingStore{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, recorder := newLoginCtx(t)
	if _, err := p.Begin(context.Background(), ctx, BeginRequest{Provider: testProvider, State: testState}); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("Begin() = %v, want ErrTransactionStore", err)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("failed Begin() set cookies: %#v", cookies)
	}
}

func TestBeginReportsEntropyFailures(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	p.random = errReader{}
	ctx, recorder := newLoginCtx(t)
	if _, err := p.Begin(context.Background(), ctx, BeginRequest{Provider: testProvider, State: testState}); err == nil {
		t.Fatal("Begin() with a failing entropy source = nil error")
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("failed Begin() set cookies: %#v", cookies)
	}
}

func TestBeginRejectsInvalidRequests(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	ctx, _ := newLoginCtx(t)
	for name, testCase := range map[string]struct {
		ctx     context.Context
		c       *web.Ctx
		request BeginRequest
		want    error
	}{
		"nil context":      {nil, ctx, BeginRequest{Provider: testProvider, State: testState}, ErrNilContext},
		"nil web ctx":      {context.Background(), nil, BeginRequest{Provider: testProvider, State: testState}, ErrInvalidRequest},
		"invalid provider": {context.Background(), ctx, BeginRequest{Provider: "../escape", State: testState}, ErrInvalidRequest},
		"short state":      {context.Background(), ctx, BeginRequest{Provider: testProvider, State: "short"}, ErrInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Begin(testCase.ctx, testCase.c, testCase.request); !errors.Is(err, testCase.want) {
				t.Fatalf("Begin() = %v, want %v", err, testCase.want)
			}
		})
	}
	var nilPlugin *Plugin
	if _, err := nilPlugin.Begin(context.Background(), ctx, BeginRequest{}); err == nil {
		t.Fatal("nil plugin Begin() = nil error")
	}
}

// --- Consume ---------------------------------------------------------------

func TestConsumeReturnsTheTransactionOnlyToTheStartingBrowser(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	stored, cookie := beginLogin(t, p, "/dashboard")

	ctx, recorder := newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	got, found, err := p.Consume(context.Background(), ctx, testProvider, testState)
	if err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if got.State != stored.State || got.Nonce != stored.Nonce || got.CodeVerifier != stored.CodeVerifier ||
		got.RequestID != stored.RequestID || got.ReturnTo != stored.ReturnTo {
		t.Fatalf("Consume() = %#v", got)
	}
	cleared := loginCookie(t, recorder, cookie.Name)
	if cleared.Value != "" || cleared.MaxAge != -1 {
		t.Fatalf("Consume() did not expire the binding cookie: %#v", cleared)
	}

	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || found {
		t.Fatalf("second Consume() found = %v, err = %v", found, err)
	}
}

func TestConsumeFromAnotherBrowserIsAMiss(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	_, cookie := beginLogin(t, p, "/dashboard")

	other := &http.Cookie{Name: cookie.Name, Value: strings.Repeat("A", 43)}
	ctx, _ := newLoginCtx(t, other)
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || found {
		t.Fatalf("Consume(other browser) found = %v, err = %v", found, err)
	}
	ctx, _ = newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	if _, found, _ := p.Consume(context.Background(), ctx, testProvider, testState); found {
		t.Fatal("Consume() found the transaction after another browser presented it")
	}
}

func TestConsumeWithoutTheCookieIsAMiss(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	beginLogin(t, p, "/dashboard")

	ctx, recorder := newLoginCtx(t)
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if cookie := loginCookie(t, recorder, "xbc_federation"); cookie.Value != "" || cookie.MaxAge != -1 {
		t.Fatalf("Consume() did not clear the binding cookie: %#v", cookie)
	}
}

func TestConsumeRejectsMalformedMaterialWithoutTouchingTheStore(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	p, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := newLoginCtx(t, &http.Cookie{Name: "xbc_federation", Value: strings.Repeat("A", 43)})
	for name, testCase := range map[string]struct{ provider, state string }{
		"provider":        {"../escape", testState},
		"short state":     {testProvider, "short"},
		"state separator": {testProvider, "0123456789abcdef:0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, found, err := p.Consume(context.Background(), ctx, testCase.provider, testCase.state); err != nil || found {
				t.Fatalf("Consume() found = %v, err = %v", found, err)
			}
		})
	}
	if store.consumes != 0 {
		t.Fatalf("store Consume calls = %d, want 0", store.consumes)
	}
}

func TestConsumeRequiresExactlyOneBindingCookie(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	p, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, cookie := beginLogin(t, p, "/dashboard")

	// Two same-name cookies are ambiguous: an intermediary or an attacker may
	// have appended one, so the callback is refused without spending the
	// transaction.
	request := httptest.NewRequest(http.MethodGet, "/federation/callback", nil)
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	request.AddCookie(&http.Cookie{Name: cookie.Name, Value: strings.Repeat("A", 43)})
	ctx := enginetest.NewCtx(httptest.NewRecorder(), request)
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || found {
		t.Fatalf("Consume(ambiguous cookies) found = %v, err = %v", found, err)
	}
	if store.consumes != 0 {
		t.Fatalf("store Consume calls = %d, want 0 for an ambiguous binding", store.consumes)
	}

	ctx, _ = newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || !found {
		t.Fatalf("Consume(single cookie) found = %v, err = %v; the ambiguous attempt must not spend the transaction", found, err)
	}
	if store.consumes != 1 {
		t.Fatalf("store Consume calls = %d, want 1", store.consumes)
	}
}

func TestConsumeHonorsExpiry(t *testing.T) {
	now := testTime(t)
	store := NewMemoryStore()
	store.now = fixedClock(&now)
	p, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(store), WithClock(fixedClock(&now)))
	if err != nil {
		t.Fatal(err)
	}
	_, cookie := beginLogin(t, p, "/dashboard")
	now = now.Add(MaxTransactionTTL + time.Second)
	ctx, _ := newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || found {
		t.Fatalf("Consume(expired) found = %v, err = %v", found, err)
	}
}

func TestConsumeRejectsUnusableContexts(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	ctx, _ := newLoginCtx(t)
	if _, _, err := p.Consume(nil, ctx, testProvider, testState); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Consume(nil context) = %v", err)
	}
	if _, _, err := p.Consume(context.Background(), nil, testProvider, testState); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Consume(nil web ctx) = %v", err)
	}
	var nilPlugin *Plugin
	if _, _, err := nilPlugin.Consume(context.Background(), ctx, testProvider, testState); err == nil {
		t.Fatal("nil plugin Consume() = nil error")
	}
}

// --- Complete --------------------------------------------------------------

func TestCompleteRevokesThePresentedSessionThenCreatesANewOne(t *testing.T) {
	sessions := &fakeSessions{}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", map[string]any{"role": "admin"})}, nil)

	ctx, recorder := newLoginCtx(t, &http.Cookie{Name: testSessionCookie, Value: "fixated-session-id"})
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/dashboard?tab=1"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got, want := operationsOf(t, sessions), []string{"revoke", "create", "set-cookie"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session operations = %v, want revoke before create before set-cookie", got)
	}
	operations := sessions.operations()
	if operations[0].id != "fixated-session-id" {
		t.Fatalf("revoked %q, want the presented session", operations[0].id)
	}
	if operations[1].subject != "local-user" {
		t.Fatalf("created session subject = %q", operations[1].subject)
	}
	attributes := operations[1].attributes
	if attributes["role"] != "admin" ||
		attributes[federationcore.AttributeProtocol] != "oidc" ||
		attributes[federationcore.AttributeProvider] != "corp" ||
		attributes[federationcore.AttributeIssuer] != "https://idp.example.com" {
		t.Fatalf("session attributes = %#v", attributes)
	}
	if _, ok := attributes[federationcore.AttributeAuthTime].(time.Time); !ok {
		t.Fatalf("auth_time attribute = %T", attributes[federationcore.AttributeAuthTime])
	}
	if operations[2].id != "new-session-id" {
		t.Fatalf("installed session = %q", operations[2].id)
	}

	if status := ctx.Writer().Status(); status != http.StatusFound {
		t.Fatalf("status = %d, want 302", status)
	}
	if location := recorder.Header().Get("Location"); location != "/dashboard?tab=1" {
		t.Fatalf("Location = %q", location)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q", cache)
	}
}

func TestCompleteRevokesEveryDistinctPresentedSessionOnce(t *testing.T) {
	sessions := &fakeSessions{}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)

	ctx, _ := newLoginCtx(t,
		&http.Cookie{Name: testSessionCookie, Value: "session-a"},
		&http.Cookie{Name: testSessionCookie, Value: "session-b"},
		&http.Cookie{Name: testSessionCookie, Value: "session-a"},
	)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatal(err)
	}
	operations := sessions.operations()
	if got, want := operationsOf(t, sessions), []string{"revoke", "revoke", "create", "set-cookie"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session operations = %v, want both distinct sessions revoked before create", got)
	}
	if operations[0].id != "session-a" || operations[1].id != "session-b" {
		t.Fatalf("revoked %q then %q, want session-a then session-b with the duplicate suppressed", operations[0].id, operations[1].id)
	}
}

func TestCompleteNeverCallsRotate(t *testing.T) {
	sessions := &fakeSessions{}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, _ := newLoginCtx(t, &http.Cookie{Name: testSessionCookie, Value: "fixated-session-id"})
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatal(err)
	}
	for _, operation := range sessions.operations() {
		if operation.op == "rotate" {
			t.Fatal("Complete() rotated a session; a login must create a fresh one")
		}
	}
}

func TestCompleteWithoutAPresentedSessionCreatesDirectly(t *testing.T) {
	sessions := &fakeSessions{}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, _ := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatal(err)
	}
	if got, want := operationsOf(t, sessions), []string{"create", "set-cookie"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session operations = %v, want %v", got, want)
	}
	if status := ctx.Writer().Status(); status != http.StatusFound {
		t.Fatalf("status = %d", status)
	}
}

func TestCompleteIgnoresUnknownOrMalformedPresentedSessions(t *testing.T) {
	for name, revokeErr := range map[string]error{
		"unknown":   sessioncore.ErrNotFound,
		"malformed": sessioncore.ErrInvalidCookie,
	} {
		t.Run(name, func(t *testing.T) {
			sessions := &fakeSessions{revokeErrors: map[string]error{"stale-id": revokeErr}}
			p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
			ctx, _ := newLoginCtx(t, &http.Cookie{Name: testSessionCookie, Value: "stale-id"})
			if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
				t.Fatal(err)
			}
			if got, want := operationsOf(t, sessions), []string{"revoke", "create", "set-cookie"}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("session operations = %v, want %v", got, want)
			}
			if status := ctx.Writer().Status(); status != http.StatusFound {
				t.Fatalf("status = %d", status)
			}
		})
	}
}

func TestCompleteFailsClosedWhenRevocationFails(t *testing.T) {
	sessions := &fakeSessions{revokeErrors: map[string]error{"fixated-session-id": errors.New("store unavailable")}}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, recorder := newLoginCtx(t, &http.Cookie{Name: testSessionCookie, Value: "fixated-session-id"})
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got, want := operationsOf(t, sessions), []string{"revoke"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session operations = %v, want only the failed revoke", got)
	}
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("error code = %q, want %q", code, CodeFailed)
	}
}

func TestCompleteRendersRefusalsWithTheFixedDeniedCode(t *testing.T) {
	logger := &capturingLogger{}
	sessions := &fakeSessions{}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Refused(federationcore.RefusalUnbound)}, nil, WithLogger(logger))
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/dashboard"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if code := failureCode(t, recorder); code != string(CodeDenied) {
		t.Fatalf("error code = %q, want %q", code, CodeDenied)
	}
	if location := recorder.Header().Get("Location"); strings.Contains(location, "alice@example.com") || strings.Contains(location, "idp-subject-1") {
		t.Fatalf("failure location leaked identity material: %q", location)
	}
	if operations := sessions.operations(); len(operations) != 0 {
		t.Fatalf("refusal touched sessions: %v", operations)
	}
	if text := logger.text(); !strings.Contains(text, "refused") || strings.Contains(text, "alice@example.com") || strings.Contains(text, "idp-subject-1") {
		t.Fatalf("refusal log = %q, want the reason but no identity material", text)
	}
}

func TestCompleteRendersResolverFailuresAsFailedWithoutLeakingDetail(t *testing.T) {
	logger := &capturingLogger{}
	sessions := &fakeSessions{}
	resolver := &stubResolver{err: errors.New("dial tcp 10.0.0.5:443: connection refused")}
	p := newLoginTestPlugin(t, sessions, resolver, nil, WithLogger(logger))
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/dashboard"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("error code = %q, want %q", code, CodeFailed)
	}
	if location := recorder.Header().Get("Location"); strings.Contains(location, "10.0.0.5") {
		t.Fatalf("failure location leaked resolver detail: %q", location)
	}
	if text := logger.text(); !strings.Contains(text, "10.0.0.5") {
		t.Fatalf("internal log omitted the resolver detail: %q", text)
	}
	if operations := sessions.operations(); len(operations) != 0 {
		t.Fatalf("resolver failure touched sessions: %v", operations)
	}
}

func TestCompleteRendersUnusableAttributesAsFailed(t *testing.T) {
	sessions := &fakeSessions{}
	resolver := &stubResolver{resolution: federationcore.Resolved("local-user", map[string]any{
		federationcore.AttributeProtocol: "saml",
	})}
	p := newLoginTestPlugin(t, sessions, resolver, nil)
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("error code = %q, want %q", code, CodeFailed)
	}
	if operations := sessions.operations(); len(operations) != 0 {
		t.Fatalf("attribute failure touched sessions: %v", operations)
	}
}

func TestCompleteRevokesANewSessionWhoseCookieCannotBeInstalled(t *testing.T) {
	sessions := &fakeSessions{nextID: "unusable-session-id", setCookieErr: errors.New("writer failed")}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got, want := operationsOf(t, sessions), []string{"create", "set-cookie", "revoke"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session operations = %v, want the unusable session revoked", got)
	}
	if operations := sessions.operations(); operations[2].id != "unusable-session-id" {
		t.Fatalf("revoked %q, want the unusable session", operations[2].id)
	}
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("error code = %q, want %q", code, CodeFailed)
	}
}

func TestCompleteRendersSessionCreationFailuresAsFailed(t *testing.T) {
	sessions := &fakeSessions{createErr: errors.New("store unavailable")}
	p := newLoginTestPlugin(t, sessions, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("error code = %q, want %q", code, CodeFailed)
	}
}

func TestCompleteSanitizesTheLandingAfterResolution(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{resolution: federationcore.Resolved("local-user", nil)}, nil)
	ctx, recorder := newLoginCtx(t)
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), "//evil.example/dashboard"); err != nil {
		t.Fatal(err)
	}
	if location := recorder.Header().Get("Location"); location != "/" {
		t.Fatalf("Location = %q, want the configured fallback", location)
	}
}

func TestCompleteRejectsUnusableContexts(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	ctx, _ := newLoginCtx(t)
	if err := p.Complete(nil, ctx, resolvedIdentity(t), "/"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Complete(nil context) = %v", err)
	}
	if err := p.Complete(context.Background(), nil, resolvedIdentity(t), "/"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Complete(nil web ctx) = %v", err)
	}
	var nilPlugin *Plugin
	if err := nilPlugin.Complete(context.Background(), ctx, resolvedIdentity(t), "/"); err == nil {
		t.Fatal("nil plugin Complete() = nil error")
	}
}

func TestCompleteNeverLeaksTransactionMaterialToTheLog(t *testing.T) {
	logger := &capturingLogger{}
	resolver := &stubResolver{resolution: federationcore.Refused(federationcore.RefusalDisabled)}
	p := newLoginTestPlugin(t, &fakeSessions{}, resolver, nil, WithLogger(logger))
	transaction, cookie := beginLogin(t, p, "/dashboard")
	ctx, recorder := newLoginCtx(t, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
	if _, found, err := p.Consume(context.Background(), ctx, testProvider, testState); err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if err := p.Complete(context.Background(), ctx, resolvedIdentity(t), transaction.ReturnTo); err != nil {
		t.Fatal(err)
	}
	text := logger.text()
	for _, secret := range []string{testState, transaction.Nonce, transaction.CodeVerifier, transaction.RequestID, cookie.Value, transaction.Binding} {
		if strings.Contains(text, secret) {
			t.Fatalf("log contains transaction material %q: %q", secret, text)
		}
	}
	if code := failureCode(t, recorder); code != string(CodeDenied) {
		t.Fatalf("error code = %q, want %q", code, CodeDenied)
	}
}

// --- Fail, ReturnTo, SetIfAbsent -------------------------------------------

func TestFailCarriesOnlyTheFixedErrorCode(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	for _, code := range []ErrorCode{CodeDenied, CodeFailed, CodeBadRequest} {
		t.Run(string(code), func(t *testing.T) {
			ctx, recorder := newLoginCtx(t)
			p.Fail(ctx, code)
			location := recorder.Header().Get("Location")
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Scheme != "" || parsed.Host != "" || parsed.Path != "/login/failed" {
				t.Fatalf("Location = %q", location)
			}
			query := parsed.Query()
			if len(query) != 1 || query.Get("error") != string(code) {
				t.Fatalf("Location query = %v, want only error=%s", query, code)
			}
			if status := ctx.Writer().Status(); status != http.StatusFound {
				t.Fatalf("status = %d", status)
			}
		})
	}

	ctx, recorder := newLoginCtx(t)
	p.Fail(ctx, ErrorCode("idp said no"))
	if code := failureCode(t, recorder); code != string(CodeFailed) {
		t.Fatalf("unknown code was not collapsed: %q", code)
	}
}

func TestFailKeepsAConfiguredFailurePath(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, func(cfg *Config) {
		cfg.FailurePath = "/auth/failed"
	})
	ctx, recorder := newLoginCtx(t)
	p.Fail(ctx, CodeDenied)
	if location := recorder.Header().Get("Location"); location != "/auth/failed?error="+string(CodeDenied) {
		t.Fatalf("Location = %q", location)
	}
}

func TestFailIgnoresUnusableContexts(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, nil)
	p.Fail(nil, CodeDenied)
	var nilPlugin *Plugin
	nilPlugin.Fail(nil, CodeDenied)
}

func TestReturnToUsesTheConfiguredPolicy(t *testing.T) {
	p := newLoginTestPlugin(t, &fakeSessions{}, &stubResolver{}, func(cfg *Config) {
		cfg.DefaultReturnTo = "/home"
	})
	if got := p.ReturnTo("//evil.example"); got != "/home" {
		t.Fatalf("ReturnTo() = %q", got)
	}
	if got := p.ReturnTo("/dashboard"); got != "/dashboard" {
		t.Fatalf("ReturnTo() = %q", got)
	}
	var nilPlugin *Plugin
	if got := nilPlugin.ReturnTo("/dashboard"); got != "/" {
		t.Fatalf("nil plugin ReturnTo() = %q", got)
	}
}

func TestSetIfAbsentDelegatesToTheStore(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	p, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	created, err := p.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || !created {
		t.Fatalf("SetIfAbsent() created = %v, err = %v", created, err)
	}
	if store.reservations != 1 {
		t.Fatalf("store reservation calls = %d", store.reservations)
	}
	if _, err := p.SetIfAbsent(nil, "saml", "assertion-id", time.Minute); !errors.Is(err, ErrNilContext) {
		t.Fatalf("SetIfAbsent(nil) = %v", err)
	}
}
