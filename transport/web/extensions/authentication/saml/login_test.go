package saml

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

func TestStartRedirectsToTheIdPWithAStoredTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, recorder := harness.start(t, "/dashboard")

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		// The redirect carries the transaction's RelayState; an intermediary
		// must not store it.
		t.Fatalf("start Cache-Control = %q, want no-store", got)
	}
	location, err := url.Parse(locationOf(recorder))
	if err != nil {
		t.Fatalf("parse IdP redirect: %v", err)
	}
	if location.Host != "idp.example.com" || location.Path != "/sso" {
		t.Fatalf("IdP redirect = %q, want https://idp.example.com/sso", location)
	}
	query := location.Query()
	if query.Get("SAMLRequest") == "" {
		t.Fatal("IdP redirect carries no SAMLRequest")
	}
	if got := query.Get("RelayState"); got != state {
		t.Fatalf("RelayState = %q, want the generated state %q", got, state)
	}
	if len(state) != 2*stateBytes {
		t.Fatalf("state = %q, want %d hexadecimal characters", state, 2*stateBytes)
	}

	if !binding.HttpOnly {
		t.Fatal("binding cookie is not HttpOnly")
	}
	if binding.Value == "" {
		t.Fatal("binding cookie carries no secret")
	}
	if got := binding.Path; got != "/" {
		t.Fatalf("binding cookie path = %q, want /", got)
	}

	// The stored transaction must bind the response to the AuthnRequest the
	// neutral service provider generated and to the sanitized landing.
	consumeRequest := newCookieCtx(t, binding)
	transaction, found, err := harness.login.Consume(context.Background(), consumeRequest, testProvider, state)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !found {
		t.Fatal("start stored no transaction")
	}
	if transaction.RequestID != requestID {
		t.Fatalf("transaction RequestID = %q, want the AuthnRequest ID %q", transaction.RequestID, requestID)
	}
	if transaction.ReturnTo != "/dashboard" {
		t.Fatalf("transaction ReturnTo = %q, want /dashboard", transaction.ReturnTo)
	}
	if transaction.Provider != testProvider || transaction.State != state {
		t.Fatalf("transaction provider/state = %q/%q", transaction.Provider, transaction.State)
	}
}

func TestStartRejectsMalformedAndUnknownProvidersAlike(t *testing.T) {
	harness := newHarness(t, nil)

	for name, provider := range map[string]string{
		"unregistered": "unknown",
		"whitespace":   "bad name",
		"dot segments": "..",
		"leading dash": "-corp",
		"overlong":     strings.Repeat("a", federationcore.MaxProviderNameLength+1),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, recorder := newStartCtx(t, provider, "")
			if err := harness.adapter.start(context.Background(), ctx); err != nil {
				t.Fatalf("start handler error = %v", err)
			}
			if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
				t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
			}
			if values := recorder.Header().Values("Set-Cookie"); len(values) != 0 {
				t.Fatalf("rejected start installed cookies: %q", values)
			}
			if location := locationOf(recorder); strings.Contains(location, provider) {
				t.Fatalf("failure location echoes the submitted provider: %q", location)
			}
			if location := locationOf(recorder); strings.HasPrefix(location, "https://idp.example.com") {
				t.Fatalf("rejected start redirected to the IdP: %q", location)
			}
		})
	}
}

// failingStore fails every transaction operation, which is the state a Redis
// outage produces.
type failingStore struct{}

func (failingStore) Create(context.Context, federationweb.Transaction) error {
	return context.DeadlineExceeded
}

func (failingStore) Consume(context.Context, string, string, string) (federationweb.Transaction, bool, error) {
	return federationweb.Transaction{}, false, context.DeadlineExceeded
}

func (failingStore) SetIfAbsent(context.Context, string, string, time.Duration) (bool, error) {
	return false, context.DeadlineExceeded
}

func TestStartDoesNotRedirectWhenTheStoreFails(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.store = failingStore{}
	})

	ctx, recorder := newStartCtx(t, testProvider, "/dashboard")
	if err := harness.adapter.start(context.Background(), ctx); err != nil {
		t.Fatalf("start handler error = %v", err)
	}
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if values := recorder.Header().Values("Set-Cookie"); len(values) != 0 {
		t.Fatalf("failed start installed cookies: %q", values)
	}
	if location := locationOf(recorder); strings.HasPrefix(location, "https:") {
		t.Fatalf("failed start redirected to the IdP: %q", location)
	}
}

func TestACSCompletesTheLoginWithAFreshSession(t *testing.T) {
	harness := newHarness(t, nil)
	loginTime := harness.clock.Now()

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	ctx, recorder := harness.acs(t, state, response, binding, &http.Cookie{Name: testSessionCookie, Value: "old-session-id"})

	if status := statusOf(ctx); status != http.StatusFound {
		t.Fatalf("acs status = %d, want 302", status)
	}
	if location := locationOf(recorder); location != "/dashboard" {
		t.Fatalf("acs location = %q, want /dashboard", location)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("acs Cache-Control = %q, want no-store", got)
	}

	sessionCookie := responseCookie(t, recorder, testSessionCookie)
	if sessionCookie.Value != "new-session-id" {
		t.Fatalf("session cookie = %q, want the newly created session", sessionCookie.Value)
	}

	operations := harness.sessions.operations()
	if len(operations) != 3 {
		t.Fatalf("session operations = %v, want revoke, create, and set-cookie", operations)
	}
	if operations[0].op != "revoke" || operations[0].id != "old-session-id" {
		t.Fatalf("first session operation = %v, want revoke(old-session-id)", operations[0])
	}
	if operations[1].op != "create" {
		t.Fatalf("second session operation = %v, want create", operations[1])
	}
	if operations[2].op != "set-cookie" || operations[2].id != "new-session-id" {
		t.Fatalf("third session operation = %v, want set-cookie(new-session-id)", operations[2])
	}
	for _, operation := range operations {
		if operation.op == "rotate" {
			t.Fatalf("login rotated a session instead of creating one: %v", operations)
		}
	}

	created, ok := harness.sessions.createdSession()
	if !ok {
		t.Fatal("no session was created")
	}
	if created.Subject != testLocalSubject {
		t.Fatalf("session subject = %q, want the local user ID %q", created.Subject, testLocalSubject)
	}
	if !created.IssuedAt.Equal(loginTime) {
		t.Fatalf("session issued at %v, want the login time %v", created.IssuedAt, loginTime)
	}
	if got, want := created.ExpiresAt, loginTime.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("session expires at %v, want a fresh absolute lifetime ending at %v", got, want)
	}
	if got := created.Attributes["idp_protocol"]; got != "saml" {
		t.Fatalf("idp_protocol attribute = %v, want saml", got)
	}
	if got := created.Attributes["idp"]; got != testProvider {
		t.Fatalf("idp attribute = %v, want %q", got, testProvider)
	}
	if got := created.Attributes["idp_issuer"]; got != testIssuer {
		t.Fatalf("idp_issuer attribute = %v, want %q", got, testIssuer)
	}
	if got := created.Attributes["role"]; got != "admin" {
		t.Fatalf("application attribute role = %v, want admin", got)
	}

	identities := harness.resolver.identities()
	if len(identities) != 1 {
		t.Fatalf("resolver saw %d identities, want 1", len(identities))
	}
	identity := identities[0]
	if identity.Protocol != "saml" || identity.Provider != testProvider {
		t.Fatalf("identity protocol/provider = %q/%q", identity.Protocol, identity.Provider)
	}
	if identity.Subject != testNameID {
		t.Fatalf("identity subject = %q, want %q", identity.Subject, testNameID)
	}
	if identity.Issuer != testIssuer {
		t.Fatalf("identity issuer = %q, want %q", identity.Issuer, testIssuer)
	}
	if got := identity.Claims["email"]; got != testMail {
		t.Fatalf("identity email claim = %v", got)
	}
}

// TestACSUsesTheConfiguredACSURLAsTheTrustAnchor pins that the expected
// destination comes from the provider configuration, never from the request
// Host an attacker controls.
func TestACSUsesTheConfiguredACSURLAsTheTrustAnchor(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(response)},
		"RelayState":   {state},
	}
	ctx, recorder := newACSRequestCtx(t, testProvider, form, binding)
	ctx.Request().Host = "attacker.example.com"
	if err := harness.adapter.acs(context.Background(), ctx); err != nil {
		t.Fatalf("acs handler error = %v", err)
	}
	if location := locationOf(recorder); location != "/dashboard" {
		t.Fatalf("acs with a foreign Host landed on %q (code %q), want /dashboard", location, failureCodeOf(t, recorder))
	}
}

func TestACSRejectsMalformedRequests(t *testing.T) {
	harness := newHarness(t, nil)
	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	encoded := base64.StdEncoding.EncodeToString(response)

	for name, form := range map[string]url.Values{
		"missing relay state": {
			"SAMLResponse": {encoded},
		},
		"missing response": {
			"RelayState": {state},
		},
		"empty response": {
			"SAMLResponse": {""}, "RelayState": {state},
		},
		"duplicate response": {
			"SAMLResponse": {encoded, encoded}, "RelayState": {state},
		},
		"duplicate relay state": {
			"SAMLResponse": {encoded}, "RelayState": {state, state},
		},
		"bad base64": {
			"SAMLResponse": {"not base64!"}, "RelayState": {state},
		},
		"unknown relay state": {
			"SAMLResponse": {encoded}, "RelayState": {strings.Repeat("a", 32)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, recorder := harness.acsValues(t, form, binding)
			if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
				t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
			}
			if operations := harness.sessions.operations(); len(operations) != 0 {
				t.Fatalf("malformed request created session work: %v", operations)
			}
		})
	}
}

// TestACSRejectsANonPostRequestWithAValidForm pins the method guard. The
// request carries a complete form and the browser-binding cookie, so removing
// the guard would let it consume the transaction and complete the login.
func TestACSRejectsANonPostRequestWithAValidForm(t *testing.T) {
	harness := newHarness(t, nil)
	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)

	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(response)},
		"RelayState":   {state},
	}
	ctx, recorder := newACSRequestCtxWithMethod(t, http.MethodGet, testProvider, form, binding)
	if err := harness.adapter.acs(context.Background(), ctx); err != nil {
		t.Fatalf("acs handler error = %v", err)
	}
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("a non-POST request created session work: %v", operations)
	}
}

// TestACSBurnsTheTransactionBeforeDecodingTheResponse pins the consume-before-
// decode order: a transaction named by a malformed SAMLResponse is spent even
// though verification never runs, so the same RelayState cannot be presented
// again with a valid assertion.
func TestACSBurnsTheTransactionBeforeDecodingTheResponse(t *testing.T) {
	harness := newHarness(t, nil)
	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)

	_, recorder := harness.acsValues(t, url.Values{
		"SAMLResponse": {"not base64!"},
		"RelayState":   {state},
	}, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("malformed response failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}

	_, recorder = harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("reused transaction failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := countOperations(harness.sessions.operations(), "create"); got != 0 {
		t.Fatalf("session creates = %d, want none", got)
	}
}

// TestACSDoesNotConsumeTheTransactionForAnEmptyFormField pins the empty-field
// guard in readACSForm. A rejected form must leave the transaction intact, so
// the same browser can still complete the login with the response it holds.
func TestACSDoesNotConsumeTheTransactionForAnEmptyFormField(t *testing.T) {
	for name, emptyField := range map[string]string{
		"empty response":    "SAMLResponse",
		"empty relay state": "RelayState",
	} {
		t.Run(name, func(t *testing.T) {
			harness := newHarness(t, nil)
			state, binding, requestID, _ := harness.start(t, "/dashboard")
			response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
			encoded := base64.StdEncoding.EncodeToString(response)

			form := url.Values{
				"SAMLResponse": {encoded},
				"RelayState":   {state},
			}
			form.Set(emptyField, "")
			_, recorder := harness.acsValues(t, form, binding)
			if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
				t.Fatalf("empty %s failure code = %q, want %q", emptyField, got, federationweb.CodeBadRequest)
			}

			_, recorder = harness.acs(t, state, response, binding)
			if location := locationOf(recorder); location != "/dashboard" {
				t.Fatalf("valid retry after an empty %s landed on %q (code %q), want /dashboard", emptyField, location, failureCodeOf(t, recorder))
			}
		})
	}
}

func TestACSRejectsAnOversizedBody(t *testing.T) {
	harness := newHarness(t, nil)
	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)

	// The form is a complete, otherwise valid login; only its total length must
	// keep it from being parsed. A doubled body bound would read it whole and
	// complete the login, so a missing length check cannot pass unnoticed.
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(response)},
		"RelayState":   {state},
	}
	form.Set("padding", "")
	form.Set("padding", strings.Repeat("a", maxFormBodyBytes-len(form.Encode())+1))
	body := form.Encode()
	if len(body) <= maxFormBodyBytes {
		t.Fatalf("test body is %d bytes, want more than the %d-byte bound", len(body), maxFormBodyBytes)
	}
	if len(body) >= 2*maxFormBodyBytes {
		t.Fatalf("test body is %d bytes, want less than twice the %d-byte bound", len(body), maxFormBodyBytes)
	}

	ctx, recorder := newRouteCtx(t, http.MethodPost, "/auth/saml/"+testProvider+"/acs", testProvider,
		strings.NewReader(body))
	ctx.Request().Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx.Request().AddCookie(binding)
	if err := harness.adapter.acs(context.Background(), ctx); err != nil {
		t.Fatalf("acs handler error = %v", err)
	}
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if location := locationOf(recorder); location == "/dashboard" {
		t.Fatal("oversized body completed the login")
	}
	if got := countOperations(harness.sessions.operations(), "create"); got != 0 {
		t.Fatalf("session creates = %d, want none", got)
	}
}

func TestACSRejectsAnOversizedDecodedResponse(t *testing.T) {
	harness := newHarness(t, nil)
	state, binding, _, _ := harness.start(t, "/dashboard")

	oversized := base64.StdEncoding.EncodeToString(make([]byte, maxDecodedResponseBytes+1))
	_, recorder := harness.acsValues(t, url.Values{
		"SAMLResponse": {oversized},
		"RelayState":   {state},
	}, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
}

func TestACSRejectsAnotherBrowser(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)

	_, recorder := harness.acs(t, state, response, &http.Cookie{Name: testBindingCookie, Value: "another-browser-secret"})
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("other-browser failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("other-browser request created session work: %v", operations)
	}

	// The mismatching request destroyed the transaction, so the browser that
	// really started the login cannot complete it later either.
	if _, recorder := harness.acs(t, state, response, binding); failureCodeOf(t, recorder) != string(federationweb.CodeBadRequest) {
		t.Fatal("the transaction survived a cross-browser request")
	}
}

func TestACSRejectsAnExpiredTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	harness.clock.Advance(5*time.Minute + time.Second)

	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("expired failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("expired request created session work: %v", operations)
	}
}

func TestACSRejectsAReplayedTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	if _, recorder := harness.acs(t, state, response, binding); locationOf(recorder) != "/dashboard" {
		t.Fatalf("first acs location = %q, want /dashboard", locationOf(recorder))
	}

	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("replay failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := countOperations(harness.sessions.operations(), "create"); got != 1 {
		t.Fatalf("session creates = %d, want exactly one", got)
	}
}

func TestACSRejectsAResponseThatDoesNotMatchTheTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	for name, mutate := range map[string]func(*responseOptions){
		"response InResponseTo": func(options *responseOptions) {
			options.responseInResponseTo = "_otherRequest0123456789"
		},
		"confirmation InResponseTo": func(options *responseOptions) {
			options.confirmationInResponseTo = "_otherRequest0123456789"
		},
		"destination": func(options *responseOptions) {
			options.destination = "https://attacker.example.com/acs"
		},
		"recipient": func(options *responseOptions) {
			options.recipient = "https://attacker.example.com/acs"
		},
		"audience": func(options *responseOptions) {
			options.audience = "https://another-sp.example.com/entity"
		},
		"issuer": func(options *responseOptions) {
			options.issuer = "https://another-idp.example.com/entity"
		},
		"status": func(options *responseOptions) {
			options.statusCode = "urn:oasis:names:tc:SAML:2.0:status:Requester"
		},
		"expired assertion": func(options *responseOptions) {
			past := time.Now().Add(-10 * time.Minute)
			options.conditionsNotBefore = &past
			options.conditionsNotOnOrAfter = &past
			options.confirmationNotBefore = &past
			options.confirmationNotOnOrAfter = &past
		},
		"untrusted signature": func(options *responseOptions) {
			options.signingKey = spKey
			options.signingCertificate = spCertificate
		},
	} {
		t.Run(name, func(t *testing.T) {
			state, binding, requestID, _ := harness.start(t, "/dashboard")
			response := buildSignedResponse(t, harness.clock.Now(), requestID, mutate)
			_, recorder := harness.acs(t, state, response, binding)
			if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
				t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
			}
			if operations := harness.sessions.operations(); len(operations) != 0 {
				t.Fatalf("invalid response created session work: %v", operations)
			}
			assertNoSensitiveText(t, locationOf(recorder))
		})
	}
}

func TestACSRejectsAReplayedAssertion(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	first := buildSignedResponse(t, harness.clock.Now(), requestID, func(options *responseOptions) {
		options.assertionID = "_assertion-replayed"
	})
	if _, recorder := harness.acs(t, state, first, binding); locationOf(recorder) != "/dashboard" {
		t.Fatalf("first acs location = %q, want /dashboard", locationOf(recorder))
	}

	// A fresh login answers a fresh AuthnRequest, but the IdP reuses the
	// assertion ID. The signature and every condition are valid; only the
	// replay cache can tell the two apart.
	state, binding, requestID, _ = harness.start(t, "/dashboard")
	second := buildSignedResponse(t, harness.clock.Now(), requestID, func(options *responseOptions) {
		options.assertionID = "_assertion-replayed"
	})
	_, recorder := harness.acs(t, state, second, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("replay failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := countOperations(harness.sessions.operations(), "create"); got != 1 {
		t.Fatalf("session creates = %d, want exactly one", got)
	}
}

func TestACSFailsClosedWhenTheTransactionStoreFails(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.store = failingStore{}
	})

	// Start cannot store a transaction over this store either, so the login
	// never leaves the adapter; posting directly still exercises the consume
	// failure path with a RelayState that looks valid.
	form := url.Values{
		"SAMLResponse": {"ZmFrZQ=="},
		"RelayState":   {strings.Repeat("a", 32)},
	}
	_, recorder := harness.acsValues(t, form, &http.Cookie{Name: testBindingCookie, Value: "secret"})
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("store failure created session work: %v", operations)
	}
}

func TestACSFailsClosedWhenTheReplayCacheIsUnavailable(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.replayStore = failingReservationStore{}
	})

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("unavailable replay cache created session work: %v", operations)
	}
}

func TestACSRendersResolverRefusalsAsDenied(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.refused = true
	})

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeDenied) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeDenied)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("refusal created session work: %v", operations)
	}
}

func TestACSTreatsAnEmptyLocalSubjectAsAFailure(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.emptySubject = true
	})

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("empty subject created session work: %v", operations)
	}
}

func TestACSRejectsApplicationAttributesThatOverrideReservedKeys(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.attributes = map[string]any{"idp_protocol": "attacker-controlled"}
	})

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	_, recorder := harness.acs(t, state, response, binding)
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("reserved-key override created session work: %v", operations)
	}
}

func TestReturnToPolicyAppliesToTheLoginLanding(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.mutateFederation = func(config *federationweb.Config) {
			config.AllowedReturnOrigins = []string{"https://app.example.com"}
		}
	})

	for name, returnTo := range map[string]string{
		"protocol relative": "//evil.example.com/steal",
		"backslash":         `/\evil.example.com`,
		"absolute":          "https://evil.example.com/steal",
		"javascript":        "javascript:alert(1)",
		"control character": "/dashboard\nSet-Cookie: x=1",
		"overlong":          "/" + strings.Repeat("a", 4096),
	} {
		t.Run(name, func(t *testing.T) {
			state, binding, requestID, _ := harness.start(t, returnTo)
			response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
			_, recorder := harness.acs(t, state, response, binding)
			if location := locationOf(recorder); location != "/" {
				t.Fatalf("hostile return_to landed on %q, want the default /", location)
			}
		})
	}

	state, binding, requestID, _ := harness.start(t, "https://app.example.com/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	_, recorder := harness.acs(t, state, response, binding)
	if location := locationOf(recorder); location != "https://app.example.com/dashboard" {
		t.Fatalf("allowlisted absolute return_to landed on %q", location)
	}
}

func TestLoginNeverLogsTransactionOrAssertionMaterial(t *testing.T) {
	harness := newHarness(t, nil)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	if _, recorder := harness.acs(t, state, response, binding); locationOf(recorder) != "/dashboard" {
		t.Fatalf("callback location = %q", locationOf(recorder))
	}

	// Force a verification failure so the error path is covered as well.
	state, binding, requestID, _ = harness.start(t, "/dashboard")
	tampered := buildSignedResponse(t, harness.clock.Now(), requestID, func(options *responseOptions) {
		options.signingKey = spKey
		options.signingCertificate = spCertificate
	})
	harness.acs(t, state, tampered, binding)

	text := harness.logger.text()
	assertNoSensitiveText(t, text)
	for name, secret := range map[string]string{
		"relay state":    state,
		"binding secret": binding.Value,
		"saml response":  base64.StdEncoding.EncodeToString(tampered)[:32],
	} {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("log carries the %s: %q", name, text)
		}
	}
}

// newCookieCtx builds a bare Ctx carrying one cookie, for direct Login calls.
func newCookieCtx(t *testing.T, cookies ...*http.Cookie) *transportweb.Ctx {
	t.Helper()
	ctx, _ := newRouteCtx(t, http.MethodGet, "/", "unused", nil)
	for _, cookie := range cookies {
		ctx.Request().AddCookie(cookie)
	}
	return ctx
}
