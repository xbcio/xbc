package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// fakeClock is an injectable clock. Tests advance it to exercise leeway, cache
// TTLs, and refresh intervals without sleeping.
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

func (c *fakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

// signingKey is one key pair published in the fake IdP's JWKS.
type signingKey struct {
	kid     string
	alg     jose.SignatureAlgorithm
	private any
	public  jose.JSONWebKey
}

// fakeIDP is a local OpenID provider: discovery, JWKS, and token endpoints,
// with request counters and knobs for the failure cases the tests exercise.
type fakeIDP struct {
	t     *testing.T
	clock *fakeClock

	server *httptest.Server
	issuer string

	clientID    string
	redirectURI string

	mu sync.Mutex
	// keys is the published key set, in order.
	keys []signingKey
	// signKid selects the signing key; empty selects the first key.
	signKid string
	// signKey overrides the signing key entirely. It can name a key that is
	// not published, which is how an unknown key ID is tested.
	signKey *signingKey

	// claimsBuilder builds the ID token claims; nil selects standardClaims.
	claimsBuilder func(idp *fakeIDP) map[string]any
	// idTokenOverride, when set, is returned verbatim instead of a signed
	// token.
	idTokenOverride string
	// nonce is echoed into the standard claims.
	nonce string
	// expectedCodeChallenge, when set, makes the token endpoint verify the
	// submitted PKCE verifier against it.
	expectedCodeChallenge string

	tokenStatus     int
	tokenRawBody    string
	tokenType       string
	omitIDToken     bool
	omitAccessToken bool
	accessToken     string
	jwksStatus      int

	// jwksBody, when set, replaces the encoded key set.
	jwksBody func() []byte
	// discoveryMutate, when set, edits the discovery document.
	discoveryMutate func(document map[string]any)

	jwksHits      int
	tokenHits     int
	discoveryHits int

	lastTokenForm          url.Values
	lastTokenAuthorization string
	lastTokenURL           string
	lastTokenMethod        string
}

// newFakeIDP starts a fake IdP with one RSA signing key.
func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{
		t:           t,
		clock:       newFakeClock(),
		clientID:    "xbc-client",
		redirectURI: "https://admin.example.com/auth/test/callback",
	}
	idp.keys = []signingKey{
		generateRSAKey(t, "rsa-1"),
		generateECKey(t, "ec-1"),
	}
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
	f.mu.Lock()
	f.discoveryHits++
	document := map[string]any{
		"issuer":                 f.issuer,
		"authorization_endpoint": f.issuer + "/authorize",
		"token_endpoint":         f.issuer + "/token",
		"jwks_uri":               f.issuer + "/jwks",
	}
	mutate := f.discoveryMutate
	f.mu.Unlock()
	if mutate != nil {
		mutate(document)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

func (f *fakeIDP) handleJWKS(w http.ResponseWriter) {
	f.mu.Lock()
	f.jwksHits++
	override := f.jwksBody
	status := f.jwksStatus
	set := jose.JSONWebKeySet{}
	for _, key := range f.keys {
		set.Keys = append(set.Keys, key.public)
	}
	f.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if override != nil {
		_, _ = w.Write(override())
		return
	}
	_ = json.NewEncoder(w).Encode(set)
}

func (f *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.tokenHits++
	f.lastTokenForm = cloneValues(r.PostForm)
	f.lastTokenAuthorization = r.Header.Get("Authorization")
	f.lastTokenURL = r.URL.String()
	f.lastTokenMethod = r.Method
	status := f.tokenStatus
	raw := f.tokenRawBody
	tokenType := f.tokenType
	omitIDToken := f.omitIDToken
	omitAccessToken := f.omitAccessToken
	accessToken := f.accessToken
	override := f.idTokenOverride
	expectedChallenge := f.expectedCodeChallenge
	key := f.signingKeyLocked()
	builder := f.claimsBuilder
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

	if tokenType == "" {
		tokenType = "Bearer"
	}
	if accessToken == "" {
		accessToken = "access-token"
	}
	claims := f.standardClaims()
	if builder != nil {
		claims = builder(f)
	}
	idToken := override
	if idToken == "" {
		idToken = f.signClaims(key, claims)
	}
	response := map[string]any{
		"token_type": tokenType,
	}
	if !omitAccessToken {
		response["access_token"] = accessToken
	}
	if !omitIDToken {
		response["id_token"] = idToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// standardClaims is the baseline claim set a test may override.
func (f *fakeIDP) standardClaims() map[string]any {
	now := f.clock.Now()
	f.mu.Lock()
	nonce := f.nonce
	f.mu.Unlock()
	return map[string]any{
		"iss":            f.issuer,
		"aud":            []string{f.clientID},
		"sub":            "idp-subject",
		"exp":            float64(now.Add(time.Hour).Unix()),
		"iat":            float64(now.Unix()),
		"nonce":          nonce,
		"auth_time":      float64(now.Add(-5 * time.Minute).Unix()),
		"email":          "person@example.com",
		"email_verified": true,
	}
}

func (f *fakeIDP) signingKeyLocked() signingKey {
	if f.signKey != nil {
		return *f.signKey
	}
	if f.signKid == "" {
		return f.keys[0]
	}
	for _, key := range f.keys {
		if key.kid == f.signKid {
			return key
		}
	}
	f.t.Fatalf("fake IdP has no signing key %q", f.signKid)
	return signingKey{}
}

// signClaims signs claims with key.
func (f *fakeIDP) signClaims(key signingKey, claims map[string]any) string {
	f.t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("marshal claims: %v", err)
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", key.kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: key.alg, Key: key.private}, options)
	if err != nil {
		f.t.Fatalf("create signer: %v", err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatalf("sign token: %v", err)
	}
	compact, err := object.CompactSerialize()
	if err != nil {
		f.t.Fatalf("serialize token: %v", err)
	}
	return compact
}

// providerConfig returns a configuration wired to this fake IdP.
func (f *fakeIDP) providerConfig() Config {
	return Config{
		Name:         "test",
		Issuer:       f.issuer,
		ClientID:     f.clientID,
		ClientSecret: outbound.Secret("client-secret"),
		RedirectURI:  f.redirectURI,
		Scopes:       []string{"openid", "profile", "email"},
		Policy:       outbound.Policy{AllowHTTP: true, AllowPrivate: true},
		Now:          f.clock.Now,
	}
}

// newProvider constructs and discovers a provider against the fake IdP.
func (f *fakeIDP) newProvider(t *testing.T) *Provider {
	t.Helper()
	provider, err := New(f.providerConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := provider.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return provider
}

// begin starts a login transaction and records the values the IdP would need
// on the callback: the expected nonce and PKCE challenge.
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
	f.setNonce(transaction.Nonce)
	f.setExpectedChallenge(parsed.Query().Get("code_challenge"))
	return transaction
}

// exchange runs one full Begin/Exchange round trip with the default claims.
func (f *fakeIDP) exchange(t *testing.T, provider *Provider) (federation.ExternalIdentity, error) {
	t.Helper()
	return provider.Exchange(context.Background(), "auth-code", f.begin(t, provider))
}

// setClaimsBuilder installs a claims override for subsequent exchanges.
func (f *fakeIDP) setClaimsBuilder(builder func(idp *fakeIDP) map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimsBuilder = builder
}

func (f *fakeIDP) counts() (jwks, token int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jwksHits, f.tokenHits
}

func (f *fakeIDP) setIDTokenOverride(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idTokenOverride = token
}

func (f *fakeIDP) setKeys(keys []signingKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeIDP) setSignKey(key *signingKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signKey = key
}

func (f *fakeIDP) setSignKid(kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signKid = kid
}

func (f *fakeIDP) setNonce(nonce string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce = nonce
}

func (f *fakeIDP) setExpectedChallenge(challenge string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expectedCodeChallenge = challenge
}

func (f *fakeIDP) setJWKSStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jwksStatus = status
}

// updateToken mutates token endpoint behavior under the lock. Tests that
// change behavior after the server has served a request use it so the race
// detector sees the same synchronization the handler does.
func (f *fakeIDP) updateToken(mutate func(*fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

// updateDiscovery mutates discovery behavior under the lock.
func (f *fakeIDP) updateDiscovery(mutate func(*fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

func (f *fakeIDP) setTokenResponse(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenStatus = status
	f.tokenRawBody = body
}

func (f *fakeIDP) lastToken() (url.Values, string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneValues(f.lastTokenForm), f.lastTokenAuthorization, f.lastTokenURL, f.lastTokenMethod
}

// publicKeyPEM returns the PKIX PEM encoding of a published key. It is the
// material an algorithm-confusion attacker would feed an HMAC verifier.
func (f *fakeIDP) publicKeyPEM(t *testing.T, kid string) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range f.keys {
		if key.kid != kid {
			continue
		}
		der, err := x509.MarshalPKIXPublicKey(key.public.Key)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}
	t.Fatalf("fake IdP has no key %q", kid)
	return nil
}

func generateRSAKey(t *testing.T, kid string) signingKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return signingKey{
		kid:     kid,
		alg:     jose.RS256,
		private: private,
		public: jose.JSONWebKey{
			Key:       &private.PublicKey,
			KeyID:     kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		},
	}
}

func generateECKey(t *testing.T, kid string) signingKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return signingKey{
		kid:     kid,
		alg:     jose.ES256,
		private: private,
		public: jose.JSONWebKey{
			Key:       &private.PublicKey,
			KeyID:     kid,
			Algorithm: string(jose.ES256),
			Use:       "sig",
		},
	}
}

// unsignedToken builds an alg=none compact token, the classic unsigned JWT an
// implementation must never accept.
func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"rsa-1"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// hmacToken signs claims with HS256 using secret, producing the algorithm
// confusion token an attacker crafts when the server treats a public key as an
// HMAC secret.
func hmacToken(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "rsa-1")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, options)
	if err != nil {
		t.Fatalf("create HMAC signer: %v", err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign HMAC token: %v", err)
	}
	compact, err := object.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize HMAC token: %v", err)
	}
	return compact
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

func cloneValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for name, entries := range values {
		clone[name] = append([]string(nil), entries...)
	}
	return clone
}

// joseKeySetOf builds a key set from public keys.
func joseKeySetOf(keys ...jose.JSONWebKey) jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: keys}
}

// joseKeySetWithSymmetric builds a key set holding one HMAC key with the given
// declared algorithm, which a verifier must never select for an asymmetric
// algorithm. An empty algorithm exercises the symmetric-key guard on its own,
// with the algorithm filter out of the way.
func joseKeySetWithSymmetric(t *testing.T, algorithm string) jose.JSONWebKeySet {
	t.Helper()
	return joseKeySetOf(jose.JSONWebKey{
		Key:       []byte(strings.Repeat("s", 32)),
		KeyID:     "shared-kid",
		Algorithm: algorithm,
		Use:       "sig",
	})
}
