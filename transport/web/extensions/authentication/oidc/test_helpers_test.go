package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
	oidccore "github.com/xbcio/xbc/extensions/authentication/oidc"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	transportweb "github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

const (
	testProvider      = "corp"
	testClientID      = "xbc-client"
	testSessionCookie = "xbc_session"
	testBindingCookie = "xbc_federation"
	testSubject       = "idp-subject-1"
	testLocalSubject  = "local-user-1"
	testAuthorization = "authorization-code"
)

// --- clock -----------------------------------------------------------------

// testClock is an injectable clock shared by the neutral OIDC client, the
// shared federation base, and the fake session manager.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// newTestClock anchors at the real current time. The shared base's memory
// store validates transaction lifetimes against its own wall clock (it has no
// injectable clock), so a fixed past instant would make every transaction
// look expired before the adapter ever stored it.
func newTestClock() *testClock {
	return &testClock{now: time.Now().UTC().Truncate(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

// --- fake IdP --------------------------------------------------------------

// fakeIDP is a local OpenID provider: discovery, JWKS, and token endpoints.
// The token endpoint verifies the PKCE verifier the neutral client sends
// against the challenge carried by the authorization URL, and echoes the nonce
// the adapter stored in its transaction.
type fakeIDP struct {
	t     *testing.T
	clock *testClock

	server *httptest.Server
	issuer string

	private *rsa.PrivateKey
	kid     string

	mu            sync.Mutex
	nonce         string
	challenge     string
	tokenCalls    int
	tokenErrorRaw string
}

func newFakeIDP(t *testing.T, clock *testClock) *fakeIDP {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	idp := &fakeIDP{t: t, clock: clock, private: private, kid: "rsa-1"}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serveHTTP))
	t.Cleanup(idp.server.Close)
	idp.issuer = idp.server.URL
	return idp
}

func (f *fakeIDP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.handleDiscovery(w)
	case "/jwks":
		f.handleJWKS(w)
	case "/token":
		f.handleToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeIDP) handleDiscovery(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                 f.issuer,
		"authorization_endpoint": f.issuer + "/authorize",
		"token_endpoint":         f.issuer + "/token",
		"jwks_uri":               f.issuer + "/jwks",
	})
}

func (f *fakeIDP) handleJWKS(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &f.private.PublicKey,
		KeyID:     f.kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}})
}

func (f *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	f.mu.Lock()
	f.tokenCalls++
	nonce := f.nonce
	challenge := f.challenge
	rawError := f.tokenErrorRaw
	f.mu.Unlock()

	if rawError != "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(rawError))
		return
	}
	if challenge != "" {
		verifier := r.PostForm.Get("code_verifier")
		if verifier == "" || oidccoreCodeChallenge(verifier) != challenge {
			http.Error(w, "pkce mismatch", http.StatusBadRequest)
			return
		}
	}

	now := f.clock.Now()
	claims := map[string]any{
		"iss":            f.issuer,
		"aud":            []string{testClientID},
		"sub":            testSubject,
		"exp":            float64(now.Add(time.Hour).Unix()),
		"iat":            float64(now.Unix()),
		"nonce":          nonce,
		"auth_time":      float64(now.Add(-time.Minute).Unix()),
		"email":          "alice@example.com",
		"email_verified": true,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Errorf("marshal claims: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", f.kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.private}, options)
	if err != nil {
		f.t.Errorf("create signer: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	object, err := signer.Sign(payload)
	if err != nil {
		f.t.Errorf("sign token: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	compact, err := object.CompactSerialize()
	if err != nil {
		f.t.Errorf("serialize token: %v", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_type":    "Bearer",
		"access_token":  "access-token",
		"id_token":      compact,
		"refresh_token": "refresh-token",
	})
}

// expect records the nonce and PKCE challenge the adapter sent to the IdP, so
// the token endpoint can validate the callback the way a real IdP would.
func (f *fakeIDP) expect(nonce, challenge string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce = nonce
	f.challenge = challenge
}

func (f *fakeIDP) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func (f *fakeIDP) setTokenError(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenErrorRaw = raw
}

// providerConfig wires a neutral OIDC provider to this fake IdP. The redirect
// URI path mirrors the adapter's default prefix so the test configuration is
// the one a deployment would register.
func (f *fakeIDP) providerConfig() oidccore.Config {
	return oidccore.Config{
		Name:         testProvider,
		Issuer:       f.issuer,
		ClientID:     testClientID,
		ClientSecret: outbound.Secret("client-secret"),
		RedirectURI:  "https://admin.example.com/auth/oidc/" + testProvider + "/callback",
		Scopes:       []string{"openid", "profile", "email"},
		Policy:       outbound.Policy{AllowHTTP: true, AllowPrivate: true},
		Now:          f.clock.Now,
	}
}

func (f *fakeIDP) newProvider(t *testing.T) *oidccore.Provider {
	t.Helper()
	provider, err := oidccore.New(f.providerConfig())
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	if err := provider.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return provider
}

// oidccoreCodeChallenge reimplements the S256 challenge the neutral client
// sends. It is test-only arithmetic on a verifier the client generated.
func oidccoreCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// --- resolver --------------------------------------------------------------

type stubResolver struct {
	mu         sync.Mutex
	resolution federationcore.Resolution
	err        error
	resolved   []federationcore.ExternalIdentity
}

func (r *stubResolver) Resolve(_ context.Context, identity federationcore.ExternalIdentity) (federationcore.Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = append(r.resolved, identity)
	if r.err != nil {
		return federationcore.Resolution{}, r.err
	}
	return r.resolution, nil
}

func (r *stubResolver) identities() []federationcore.ExternalIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]federationcore.ExternalIdentity(nil), r.resolved...)
}

// --- sessions --------------------------------------------------------------

type sessionOperation struct {
	op      string
	id      string
	subject string
}

// fakeSessions records the exact lifecycle calls the shared base makes. Rotate
// is an error: a login must create a new session, never rotate the presented
// one.
type fakeSessions struct {
	mu           sync.Mutex
	ops          []sessionOperation
	now          func() time.Time
	ttl          time.Duration
	nextID       string
	revokeErrors map[string]error
	createErr    error
	setCookieErr error
	created      sessioncore.Session
}

func newFakeSessions(clock *testClock) *fakeSessions {
	return &fakeSessions{now: clock.Now, ttl: 24 * time.Hour, nextID: "new-session-id"}
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
	now := s.now().UTC()
	value := sessioncore.Session{
		ID:         s.nextID,
		Subject:    subject,
		Attributes: attributes,
		IssuedAt:   now,
		ExpiresAt:  now.Add(s.ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, sessionOperation{op: "create", id: value.ID, subject: subject})
	if s.createErr != nil {
		return sessioncore.Session{}, s.createErr
	}
	s.created = value
	return value, nil
}

func (s *fakeSessions) Rotate(_ context.Context, id string) (sessioncore.Session, error) {
	s.record(sessionOperation{op: "rotate", id: id})
	return sessioncore.Session{}, errors.New("rotate must not be called during a login")
}

func (s *fakeSessions) Revoke(_ context.Context, id string) error {
	s.record(sessionOperation{op: "revoke", id: id})
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *fakeSessions) SetCookie(c *transportweb.Ctx, value sessioncore.Session) error {
	s.record(sessionOperation{op: "set-cookie", id: value.ID})
	s.mu.Lock()
	setCookieErr := s.setCookieErr
	s.mu.Unlock()
	if setCookieErr != nil {
		return setCookieErr
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     testSessionCookie,
		Value:    value.ID,
		Path:     "/",
		HttpOnly: true,
	})
	return nil
}

func (s *fakeSessions) ClearCookie(c *transportweb.Ctx) {
	s.record(sessionOperation{op: "clear-cookie"})
	http.SetCookie(c.Writer(), &http.Cookie{Name: testSessionCookie, Value: "", MaxAge: -1})
}

func (s *fakeSessions) createdSession() (sessioncore.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.created.ID == "" {
		return sessioncore.Session{}, false
	}
	return s.created, true
}

// --- logger ----------------------------------------------------------------

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

// --- context helpers -------------------------------------------------------

func newStartCtx(t *testing.T, provider, returnTo string) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/auth/oidc/" + url.PathEscape(provider) + "/start"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	return newRouteCtx(t, target, provider)
}

func newCallbackCtx(t *testing.T, provider, rawQuery string, cookies ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/auth/oidc/" + url.PathEscape(provider) + "/callback"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	ctx, recorder := newRouteCtx(t, target, provider)
	for _, cookie := range cookies {
		ctx.Request().AddCookie(cookie)
	}
	return ctx, recorder
}

func newRouteCtx(t *testing.T, target, provider string) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.SetPathValue("provider", provider)
	recorder := httptest.NewRecorder()
	return enginetest.NewCtx(recorder, request), recorder
}

func responseCookie(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response set no %q cookie; Set-Cookie = %q", name, recorder.Header().Values("Set-Cookie"))
	return nil
}

func statusOf(ctx *transportweb.Ctx) int {
	return ctx.Writer().Status()
}

func locationOf(recorder *httptest.ResponseRecorder) string {
	return recorder.Header().Get("Location")
}

// --- adapter construction --------------------------------------------------

type adapterHarness struct {
	clock            *testClock
	idp              *fakeIDP
	provider         *oidccore.Provider
	registry         *federationcore.Registry[*oidccore.Provider]
	resolver         *stubResolver
	sessions         *fakeSessions
	logger           *capturingLogger
	login            federationweb.Login
	adapter          *Plugin
	federationConfig federationweb.Config
}

type harnessOptions struct {
	mutateFederation func(*federationweb.Config)
	mutateAdapter    func(*Config)
	store            federationweb.Store
	refused          bool
	emptySubject     bool
	attributes       map[string]any
	resolverError    error
}

func newHarness(t *testing.T, mutate func(*harnessOptions)) *adapterHarness {
	t.Helper()
	options := harnessOptions{}
	if mutate != nil {
		mutate(&options)
	}

	clock := newTestClock()
	idp := newFakeIDP(t, clock)
	provider := idp.newProvider(t)
	registry := oidccore.NewRegistry()
	if err := registry.Put(testProvider, provider); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}

	resolution := federationcore.Resolved(testLocalSubject, map[string]any{"role": "admin"})
	if options.emptySubject {
		resolution = federationcore.Resolved("", nil)
	}
	if options.attributes != nil {
		resolution = federationcore.Resolved(testLocalSubject, options.attributes)
	}
	if options.refused {
		resolution = federationcore.Refused(federationcore.RefusalUnbound)
	}
	resolver := &stubResolver{resolution: resolution, err: options.resolverError}
	sessions := newFakeSessions(clock)
	logger := &capturingLogger{}

	federationConfig := federationweb.DefaultConfig()
	federationConfig.Backend = federationweb.BackendMemory
	federationConfig.CookieSecure = false // httptest requests are plain HTTP
	federationConfig.TransactionTTL = 5 * time.Minute
	if options.mutateFederation != nil {
		options.mutateFederation(&federationConfig)
	}
	store := options.store
	if store == nil {
		store = federationweb.NewMemoryStore()
	}
	login, err := federationweb.New(
		federationConfig,
		resolver,
		sessions,
		testSessionCookie,
		federationweb.WithStore(store),
		federationweb.WithClock(clock.Now),
	)
	if err != nil {
		t.Fatalf("federation.New: %v", err)
	}

	adapterConfig := DefaultConfig()
	if options.mutateAdapter != nil {
		options.mutateAdapter(&adapterConfig)
	}
	adapter, err := New(adapterConfig, registry, login, WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &adapterHarness{
		clock:            clock,
		idp:              idp,
		provider:         provider,
		registry:         registry,
		resolver:         resolver,
		sessions:         sessions,
		logger:           logger,
		login:            login,
		adapter:          adapter,
		federationConfig: federationConfig,
	}
}

// start runs the start handler and returns the authorization URL it redirected
// to together with the browser-binding cookie.
func (h *adapterHarness) start(t *testing.T, returnTo string) (*url.URL, *http.Cookie) {
	t.Helper()
	authorization, binding, _ := h.startResponse(t, returnTo)
	return authorization, binding
}

// startResponse is start with the response recorder retained, for tests that
// assert the properties of the redirect itself.
func (h *adapterHarness) startResponse(t *testing.T, returnTo string) (*url.URL, *http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	ctx, recorder := newStartCtx(t, testProvider, returnTo)
	if err := h.adapter.start(context.Background(), ctx); err != nil {
		t.Fatalf("start handler error = %v", err)
	}
	if status := statusOf(ctx); status != http.StatusFound {
		t.Fatalf("start status = %d, want 302 (Location = %q)", status, locationOf(recorder))
	}
	location := locationOf(recorder)
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse authorization URL %q: %v", location, err)
	}
	h.idp.expect(parsed.Query().Get("nonce"), parsed.Query().Get("code_challenge"))
	return parsed, responseCookie(t, recorder, testBindingCookie), recorder
}

// callback runs the callback handler for the authorization URL start returned.
func (h *adapterHarness) callback(t *testing.T, authorization *url.URL, binding *http.Cookie, query string, extra ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	if query == "" {
		query = "state=" + url.QueryEscape(authorization.Query().Get("state")) +
			"&code=" + url.QueryEscape(testAuthorization)
	}
	cookies := append([]*http.Cookie{binding}, extra...)
	ctx, recorder := newCallbackCtx(t, testProvider, query, cookies...)
	if err := h.adapter.callback(context.Background(), ctx); err != nil {
		t.Fatalf("callback handler error = %v", err)
	}
	return ctx, recorder
}

func failureCodeOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	location := locationOf(recorder)
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse failure location %q: %v", location, err)
	}
	if got := parsed.Path; got != "/login/failed" {
		t.Fatalf("failure landing = %q, want /login/failed", got)
	}
	return parsed.Query().Get("error")
}
