package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

const (
	testProvider          = "corp"
	testClientID          = "xbc-client"
	testClientSecret      = "client-secret"
	testRedirectURI       = "https://admin.example.com/auth/oauth2/corp/callback"
	testAuthorizationCode = "authorization-code"
	testAccessToken       = "access-token"
	testSubject           = "idp-subject-1"
)

// fakeClock is an injectable clock; AuthTime assertions use it so no test
// depends on the wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// fakeIDP is a local OAuth 2.0 provider: a token endpoint and a userinfo
// endpoint, with request counters and knobs for the failure cases the tests
// exercise. The authorization endpoint is never served: Begin builds its URL
// locally and no test follows it.
type fakeIDP struct {
	t     *testing.T
	clock *fakeClock

	server *httptest.Server
	issuer string

	mu sync.Mutex
	// expectedCodeChallenge, when set, makes the token endpoint verify the
	// submitted PKCE verifier against it.
	expectedCodeChallenge string

	tokenStatus     int
	tokenRawBody    string
	tokenType       string
	omitAccessToken bool
	accessToken     string

	userInfoStatus   int
	userInfoRawBody  string
	userInfoJSON     map[string]any
	userInfoRedirect string

	tokenHits    int
	userInfoHits int
	redirectHits int

	lastTokenForm          url.Values
	lastTokenAuthorization string
	lastTokenMethod        string
	lastTokenURL           string

	lastUserInfoAuthorization string
	lastUserInfoURL           string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{t: t, clock: newFakeClock()}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serveHTTP))
	t.Cleanup(idp.server.Close)
	idp.issuer = idp.server.URL
	return idp
}

func (f *fakeIDP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		f.handleToken(w, r)
	case "/userinfo":
		f.handleUserInfo(w, r)
	case "/redirected":
		f.mu.Lock()
		f.redirectHits++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sub":"redirected-subject"}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	f.mu.Lock()
	f.tokenHits++
	f.lastTokenForm = cloneValues(r.PostForm)
	f.lastTokenAuthorization = r.Header.Get("Authorization")
	f.lastTokenMethod = r.Method
	f.lastTokenURL = r.URL.String()
	status := f.tokenStatus
	raw := f.tokenRawBody
	tokenType := f.tokenType
	omitAccessToken := f.omitAccessToken
	accessToken := f.accessToken
	expectedChallenge := f.expectedCodeChallenge
	f.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, raw)
		return
	}
	if expectedChallenge != "" {
		verifier := r.PostForm.Get("code_verifier")
		if verifier == "" || codeChallenge(verifier) != expectedChallenge {
			http.Error(w, "pkce mismatch", http.StatusBadRequest)
			return
		}
	}
	if raw != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
		return
	}

	if tokenType == "" {
		tokenType = "Bearer"
	}
	if accessToken == "" {
		accessToken = testAccessToken
	}
	response := map[string]any{"token_type": tokenType}
	if !omitAccessToken {
		response["access_token"] = accessToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (f *fakeIDP) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.userInfoHits++
	f.lastUserInfoAuthorization = r.Header.Get("Authorization")
	f.lastUserInfoURL = r.URL.String()
	redirect := f.userInfoRedirect
	status := f.userInfoStatus
	raw := f.userInfoRawBody
	document := f.userInfoJSON
	f.mu.Unlock()

	if redirect != "" {
		w.Header().Set("Location", redirect)
		w.WriteHeader(http.StatusFound)
		return
	}
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if raw != "" {
		_, _ = io.WriteString(w, raw)
		return
	}
	if document == nil {
		document = f.defaultUserInfo()
	}
	_ = json.NewEncoder(w).Encode(document)
}

// defaultUserInfo is the baseline account document: a stable subject, an
// email address that must never become the subject, and a nested display name.
func (f *fakeIDP) defaultUserInfo() map[string]any {
	return map[string]any{
		"sub":   testSubject,
		"email": "alice@example.com",
		"profile": map[string]any{
			"name": "Alice",
		},
	}
}

// providerConfig wires a neutral provider to this fake IdP.
func (f *fakeIDP) providerConfig() Config {
	return Config{
		Name:                  testProvider,
		Issuer:                f.issuer,
		AuthorizationEndpoint: f.issuer + "/authorize",
		TokenEndpoint:         f.issuer + "/token",
		UserInfoEndpoint:      f.issuer + "/userinfo",
		ClientID:              testClientID,
		ClientSecret:          outbound.Secret(testClientSecret),
		RedirectURI:           testRedirectURI,
		Scopes:                []string{"profile", "email"},
		SubjectField:          "sub",
		ClaimFields:           map[string]string{"email": "email", "name": "profile.name"},
		Policy:                outbound.Policy{AllowHTTP: true, AllowPrivate: true},
		Now:                   f.clock.Now,
	}
}

func (f *fakeIDP) newProvider(t *testing.T) *Provider {
	t.Helper()
	provider, err := New(f.providerConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// begin starts a login transaction and records the PKCE challenge, so the
// token endpoint can verify the verifier the way a real IdP would.
func (f *fakeIDP) begin(t *testing.T, provider *Provider) AuthorizationRequest {
	t.Helper()
	transaction, err := provider.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(transaction.URL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	f.mu.Lock()
	f.expectedCodeChallenge = parsed.Query().Get("code_challenge")
	f.mu.Unlock()
	return transaction
}

// exchange runs one full Begin/Exchange round trip with the default document.
func (f *fakeIDP) exchange(t *testing.T, provider *Provider) (federation.ExternalIdentity, error) {
	t.Helper()
	return provider.Exchange(context.Background(), testAuthorizationCode, f.begin(t, provider))
}

func (f *fakeIDP) counts() (token, userInfo, redirect int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenHits, f.userInfoHits, f.redirectHits
}

func (f *fakeIDP) setToken(mutate func(*fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

func (f *fakeIDP) setUserInfo(mutate func(*fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

func (f *fakeIDP) lastToken() (url.Values, string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneValues(f.lastTokenForm), f.lastTokenAuthorization, f.lastTokenMethod, f.lastTokenURL
}

func (f *fakeIDP) lastUserInfo() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUserInfoAuthorization, f.lastUserInfoURL
}

func cloneValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for name, entries := range values {
		clone[name] = append([]string(nil), entries...)
	}
	return clone
}

func requireErrorIs(t *testing.T, err, target error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error wrapping %v, got nil", what, target)
	}
	if !errors.Is(err, target) {
		t.Fatalf("%s: expected an error wrapping %v, got %v", what, target, err)
	}
}

func requireNoError(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}
