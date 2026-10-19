package saml

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	transportweb "github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

const (
	testProvider      = "corp"
	testSessionCookie = "xbc_session"
	testBindingCookie = "xbc_federation"
	testNameID        = "user-12345"
	testMail          = "alice@example.com"
	testLocalSubject  = "local-user-1"
	testIssuer        = "https://idp.example.com/entity"
	testEntityID      = "https://admin.example.com/saml/entity"
	testSSOURL        = "https://idp.example.com/sso"
	testACSURL        = "https://admin.example.com/auth/saml/" + testProvider + "/acs"
)

// --- certificates ----------------------------------------------------------

var (
	idpKey         *rsa.PrivateKey
	idpCertificate *x509.Certificate
	spKey          *rsa.PrivateKey
	spCertificate  *x509.Certificate
)

var certificateSerial atomic.Int64

func TestMain(m *testing.M) {
	idpKey, idpCertificate = mustTestKeyPair("idp")
	spKey, spCertificate = mustTestKeyPair("sp")
	os.Exit(m.Run())
}

// mustTestKeyPair generates one self-signed RSA key pair valid around the
// current wall clock.
func mustTestKeyPair(name string) (*rsa.PrivateKey, *x509.Certificate) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("generate %s key: %v", name, err))
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(certificateSerial.Add(1)),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(fmt.Sprintf("create %s certificate: %v", name, err))
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		panic(fmt.Sprintf("parse %s certificate: %v", name, err))
	}
	return key, certificate
}

// testKeyStore adapts a key and certificate to the goxmldsig key-store shape.
type testKeyStore struct {
	key         *rsa.PrivateKey
	certificate []byte
}

func (s testKeyStore) GetKeyPair() (*rsa.PrivateKey, []byte, error) {
	return s.key, s.certificate, nil
}

// --- clock -----------------------------------------------------------------

// testClock is an injectable clock shared by the neutral service provider, the
// shared federation base, and the fake session manager.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// newTestClock anchors at the real current time. The shared base's memory store
// validates transaction lifetimes against its own wall clock (it has no
// injectable clock), so a fixed past instant would make every transaction look
// expired before the adapter ever stored it.
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
func (l *capturingLogger) LogValue() slog.Value        { return slog.StringValue("capturing") }

func (l *capturingLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var builder strings.Builder
	for _, entry := range l.entries {
		fmt.Fprintf(&builder, "%s %s %v\n", entry.level, entry.msg, entry.kv)
	}
	return builder.String()
}

// --- SAML response fixture -------------------------------------------------

// responseSerial makes the default response and assertion IDs distinct across
// one process run, so a test that builds several valid responses does not
// accidentally exercise replay detection.
var responseSerial atomic.Int64

type testAttribute struct {
	name   string
	values []string
}

// responseOptions describes one synthetic SAML response. The zero value is
// replaced by the defaults in buildSignedResponse.
type responseOptions struct {
	responseID           string
	assertionID          string
	responseInResponseTo string
	destination          string
	statusCode           string
	issuer               string
	nameID               string
	nameIDFormat         string

	recipient                string
	confirmationInResponseTo string
	confirmationNotBefore    *time.Time
	confirmationNotOnOrAfter *time.Time
	conditionsNotBefore      *time.Time
	conditionsNotOnOrAfter   *time.Time
	audience                 string
	attributes               []testAttribute

	signingKey         *rsa.PrivateKey
	signingCertificate *x509.Certificate
}

func responseDefaults(now time.Time, requestID string) responseOptions {
	serial := responseSerial.Add(1)
	notBefore := now.Add(-5 * time.Minute)
	notOnOrAfter := now.Add(5 * time.Minute)
	return responseOptions{
		responseID:               fmt.Sprintf("_response%032d", serial),
		assertionID:              fmt.Sprintf("_assertion%032d", serial),
		responseInResponseTo:     requestID,
		destination:              testACSURL,
		statusCode:               "urn:oasis:names:tc:SAML:2.0:status:Success",
		issuer:                   testIssuer,
		nameID:                   testNameID,
		nameIDFormat:             samlcore.NameIDFormatPersistent,
		recipient:                testACSURL,
		confirmationInResponseTo: requestID,
		confirmationNotBefore:    &notBefore,
		confirmationNotOnOrAfter: &notOnOrAfter,
		conditionsNotBefore:      &notBefore,
		conditionsNotOnOrAfter:   &notOnOrAfter,
		audience:                 testEntityID,
		attributes: []testAttribute{
			{name: "mail", values: []string{testMail}},
		},
		signingKey:         idpKey,
		signingCertificate: idpCertificate,
	}
}

// buildSignedResponse renders, signs, and serializes the described response.
func buildSignedResponse(t *testing.T, now time.Time, requestID string, mutate func(*responseOptions)) []byte {
	t.Helper()
	options := responseDefaults(now, requestID)
	if mutate != nil {
		mutate(&options)
	}
	document := etree.NewDocument()
	document.ReadSettings = etree.ReadSettings{PreserveDuplicateAttrs: true}
	if err := document.ReadFromString(options.xml()); err != nil {
		t.Fatalf("parse test response: %v", err)
	}
	assertion := findTestElement(t, document.Root(), samlcoreAssertionNamespace, "Assertion")
	signed := signTestElement(t, assertion, options)
	parent := assertion.Parent()
	index := assertion.Index()
	parent.RemoveChildAt(index)
	parent.InsertChildAt(index, signed)
	data, err := document.WriteToBytes()
	if err != nil {
		t.Fatalf("serialize test response: %v", err)
	}
	return data
}

// samlcoreAssertionNamespace mirrors the assertion namespace the fixture must
// emit. The neutral module keeps the constant unexported.
const samlcoreAssertionNamespace = "urn:oasis:names:tc:SAML:2.0:assertion"

const samlcoreProtocolNamespace = "urn:oasis:names:tc:SAML:2.0:protocol"

const samlcoreDSigNamespace = "http://www.w3.org/2000/09/xmldsig#"

func (o responseOptions) xml() string {
	var builder strings.Builder
	builder.WriteString(`<samlp:Response xmlns:samlp="` + samlcoreProtocolNamespace + `" xmlns:saml="` + samlcoreAssertionNamespace + `"`)
	fmt.Fprintf(&builder, ` ID="%s" Version="2.0" IssueInstant="%s"`, o.responseID, time.Now().UTC().Format(time.RFC3339))
	if o.destination != "" {
		fmt.Fprintf(&builder, ` Destination="%s"`, o.destination)
	}
	if o.responseInResponseTo != "" {
		fmt.Fprintf(&builder, ` InResponseTo="%s"`, o.responseInResponseTo)
	}
	builder.WriteString(">")
	fmt.Fprintf(&builder, `<saml:Issuer>%s</saml:Issuer>`, o.issuer)
	fmt.Fprintf(&builder, `<samlp:Status><samlp:StatusCode Value="%s"/></samlp:Status>`, o.statusCode)
	builder.WriteString(o.assertionXML())
	builder.WriteString(`</samlp:Response>`)
	return builder.String()
}

func (o responseOptions) assertionXML() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, `<saml:Assertion xmlns:saml="%s" xmlns:samlp="%s" ID="%s" Version="2.0" IssueInstant="%s">`,
		samlcoreAssertionNamespace, samlcoreProtocolNamespace, o.assertionID, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&builder, `<saml:Issuer>%s</saml:Issuer>`, o.issuer)
	builder.WriteString(`<saml:Subject>`)
	fmt.Fprintf(&builder, `<saml:NameID Format="%s">%s</saml:NameID>`, o.nameIDFormat, o.nameID)
	builder.WriteString(`<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData`)
	if o.recipient != "" {
		fmt.Fprintf(&builder, ` Recipient="%s"`, o.recipient)
	}
	if o.confirmationInResponseTo != "" {
		fmt.Fprintf(&builder, ` InResponseTo="%s"`, o.confirmationInResponseTo)
	}
	if o.confirmationNotBefore != nil {
		fmt.Fprintf(&builder, ` NotBefore="%s"`, o.confirmationNotBefore.UTC().Format(time.RFC3339))
	}
	if o.confirmationNotOnOrAfter != nil {
		fmt.Fprintf(&builder, ` NotOnOrAfter="%s"`, o.confirmationNotOnOrAfter.UTC().Format(time.RFC3339))
	}
	builder.WriteString(`/></saml:SubjectConfirmation></saml:Subject>`)
	builder.WriteString(`<saml:Conditions`)
	if o.conditionsNotBefore != nil {
		fmt.Fprintf(&builder, ` NotBefore="%s"`, o.conditionsNotBefore.UTC().Format(time.RFC3339))
	}
	if o.conditionsNotOnOrAfter != nil {
		fmt.Fprintf(&builder, ` NotOnOrAfter="%s"`, o.conditionsNotOnOrAfter.UTC().Format(time.RFC3339))
	}
	builder.WriteString(`>`)
	fmt.Fprintf(&builder, `<saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction>`, o.audience)
	builder.WriteString(`</saml:Conditions>`)
	builder.WriteString(`<saml:AuthnStatement AuthnInstant="` + time.Now().UTC().Format(time.RFC3339) + `" SessionIndex="session-1"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>`)
	builder.WriteString(`<saml:AttributeStatement>`)
	for _, attribute := range o.attributes {
		fmt.Fprintf(&builder, `<saml:Attribute Name="%s">`, attribute.name)
		for _, value := range attribute.values {
			fmt.Fprintf(&builder, `<saml:AttributeValue>%s</saml:AttributeValue>`, value)
		}
		builder.WriteString(`</saml:Attribute>`)
	}
	builder.WriteString(`</saml:AttributeStatement>`)
	builder.WriteString(`</saml:Assertion>`)
	return builder.String()
}

// signTestElement signs target with the configured key and certificate.
func signTestElement(t *testing.T, target *etree.Element, options responseOptions) *etree.Element {
	t.Helper()
	context := dsig.NewDefaultSigningContext(testKeyStore{
		key:         options.signingKey,
		certificate: options.signingCertificate.Raw,
	})
	signed, err := context.SignEnveloped(target)
	if err != nil {
		t.Fatalf("sign test element: %v", err)
	}
	// SignEnveloped leaves the cached parent of the appended Signature stale;
	// re-attaching it through AddChild makes the document well-formed.
	for position, child := range signed.ChildElements() {
		if child.Tag == "Signature" && child.NamespaceURI() == samlcoreDSigNamespace {
			signed.RemoveChildAt(position)
			signed.AddChild(child)
			break
		}
	}
	return signed
}

func findTestElement(t *testing.T, root *etree.Element, namespace, tag string) *etree.Element {
	t.Helper()
	var matches []*etree.Element
	var walk func(*etree.Element)
	walk = func(element *etree.Element) {
		for _, child := range element.ChildElements() {
			if child.Tag == tag && child.NamespaceURI() == namespace {
				matches = append(matches, child)
			}
			walk(child)
		}
	}
	walk(root)
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %s element, found %d", tag, len(matches))
	}
	return matches[0]
}

// --- context helpers -------------------------------------------------------

func newStartCtx(t *testing.T, provider, returnTo string) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/auth/saml/" + url.PathEscape(provider) + "/start"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	return newRouteCtx(t, http.MethodGet, target, provider, nil)
}

func newACSRequestCtx(t *testing.T, provider string, form url.Values, cookies ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	return newACSRequestCtxWithMethod(t, http.MethodPost, provider, form, cookies...)
}

// newACSRequestCtxWithMethod builds an ACS request with an arbitrary method and
// a form-encoded body. GET is deliberately allowed so the method guard can be
// exercised against a request that would otherwise complete the login.
func newACSRequestCtxWithMethod(t *testing.T, method, provider string, form url.Values, cookies ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/auth/saml/" + url.PathEscape(provider) + "/acs"
	request := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Host = "admin.example.com"
	request.SetPathValue("provider", provider)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	return enginetest.NewCtx(recorder, request), recorder
}

func newMetadataCtx(t *testing.T, provider string) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/auth/saml/" + url.PathEscape(provider) + "/metadata"
	return newRouteCtx(t, http.MethodGet, target, provider, nil)
}

func newRouteCtx(t *testing.T, method, target, provider string, body io.Reader) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
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

// --- adapter construction --------------------------------------------------

type adapterHarness struct {
	clock            *testClock
	provider         *samlcore.Provider
	registry         *federationcore.Registry[*samlcore.Provider]
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
	mutateProvider   func(*samlcore.Config)
	store            federationweb.Store
	replayStore      ReservationStore
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

	replayStore := options.replayStore
	if replayStore == nil {
		replayStore = login
	}
	replayCache, err := NewReplayCache(replayStore, testProvider, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}

	providerConfig := samlcore.Config{
		Name:            testProvider,
		Issuer:          testIssuer,
		SSOURL:          testSSOURL,
		EntityID:        testEntityID,
		ACSURL:          testACSURL,
		IdPCertificates: []*x509.Certificate{idpCertificate},
		ClaimAttributes: map[string]string{"email": "mail"},
		ReplayCache:     replayCache,
		Now:             clock.Now,
	}
	if options.mutateProvider != nil {
		options.mutateProvider(&providerConfig)
	}
	provider, err := samlcore.New(providerConfig)
	if err != nil {
		t.Fatalf("saml.New: %v", err)
	}
	registry := samlcore.NewRegistry()
	if err := registry.Put(testProvider, provider); err != nil {
		t.Fatalf("registry.Put: %v", err)
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

// start runs the start handler and returns the RelayState, the binding cookie,
// the AuthnRequest ID the IdP must echo, and the recorder.
func (h *adapterHarness) start(t *testing.T, returnTo string) (string, *http.Cookie, string, *httptest.ResponseRecorder) {
	t.Helper()
	ctx, recorder := newStartCtx(t, testProvider, returnTo)
	if err := h.adapter.start(context.Background(), ctx); err != nil {
		t.Fatalf("start handler error = %v", err)
	}
	if status := statusOf(ctx); status != http.StatusFound {
		t.Fatalf("start status = %d, want 302 (Location = %q)", status, locationOf(recorder))
	}
	location, err := url.Parse(locationOf(recorder))
	if err != nil {
		t.Fatalf("parse IdP redirect %q: %v", locationOf(recorder), err)
	}
	state := location.Query().Get("RelayState")
	if state == "" {
		t.Fatal("start redirect carries no RelayState")
	}
	requestID := authnRequestID(t, location.Query().Get("SAMLRequest"))
	binding := responseCookie(t, recorder, testBindingCookie)
	return state, binding, requestID, recorder
}

// acs posts one encoded response with the given RelayState and cookies.
func (h *adapterHarness) acs(t *testing.T, relayState string, response []byte, cookies ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(response)},
		"RelayState":   {relayState},
	}
	return h.acsValues(t, form, cookies...)
}

// acsValues posts an arbitrary form, for the malformed-request tests.
func (h *adapterHarness) acsValues(t *testing.T, form url.Values, cookies ...*http.Cookie) (*transportweb.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	ctx, recorder := newACSRequestCtx(t, testProvider, form, cookies...)
	if err := h.adapter.acs(context.Background(), ctx); err != nil {
		t.Fatalf("acs handler error = %v", err)
	}
	return ctx, recorder
}

// authnRequestID extracts the ID from an HTTP-Redirect binding SAMLRequest.
func authnRequestID(t *testing.T, encoded string) string {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode SAMLRequest: %v", err)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()
	documentBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("inflate SAMLRequest: %v", err)
	}
	document := etree.NewDocument()
	if err := document.ReadFromBytes(documentBytes); err != nil {
		t.Fatalf("parse SAMLRequest: %v", err)
	}
	root := document.Root()
	if root == nil {
		t.Fatal("SAMLRequest has no root element")
	}
	id := root.SelectAttrValue("ID", "")
	if id == "" {
		t.Fatal("SAMLRequest carries no ID")
	}
	return id
}

func countOperations(operations []sessionOperation, name string) int {
	count := 0
	for _, operation := range operations {
		if operation.op == name {
			count++
		}
	}
	return count
}

// failingReservationStore fails every claim, which is the state a Redis outage
// produces.
type failingReservationStore struct{}

func (failingReservationStore) SetIfAbsent(context.Context, string, string, time.Duration) (bool, error) {
	return false, context.DeadlineExceeded
}

// recordingReservationStore records every reservation for assertions about
// namespace, key, and TTL.
type recordingReservationStore struct {
	mu   sync.Mutex
	call []recordedReservation
}

type recordedReservation struct {
	namespace string
	key       string
	ttl       time.Duration
}

func (s *recordingReservationStore) SetIfAbsent(_ context.Context, namespace, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call = append(s.call, recordedReservation{namespace: namespace, key: key, ttl: ttl})
	return true, nil
}

func (s *recordingReservationStore) reservations() []recordedReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedReservation(nil), s.call...)
}

// assertNoSensitiveText checks that a diagnostic carries no assertion content,
// NameID, attribute value, certificate, or key material.
func assertNoSensitiveText(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{
		testNameID,
		testMail,
		"BEGIN PRIVATE KEY",
		"BEGIN RSA PRIVATE KEY",
		base64.StdEncoding.EncodeToString(idpCertificate.Raw),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("log or error %q contains sensitive value %q", text, secret)
		}
	}
}
