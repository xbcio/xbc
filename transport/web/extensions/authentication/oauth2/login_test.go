package oauth2

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	oauth2core "github.com/xbcio/xbc/extensions/authentication/oauth2"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

func TestStartRedirectsToTheProviderWithAStoredTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding, recorder := harness.startResponse(t, "/dashboard")

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		// The redirect carries the transaction state; an intermediary must not
		// store it.
		t.Fatalf("start Cache-Control = %q, want no-store", got)
	}
	if authorization.Host != strings.TrimPrefix(harness.idp.issuer, "http://") {
		t.Fatalf("authorization host = %q, want the fake IdP host", authorization.Host)
	}
	if authorization.Path != "/authorize" {
		t.Fatalf("authorization path = %q, want /authorize", authorization.Path)
	}
	query := authorization.Query()
	if got := query.Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if got := query.Get("client_id"); got != testClientID {
		t.Fatalf("client_id = %q, want %q", got, testClientID)
	}
	if got := query.Get("redirect_uri"); got != "https://admin.example.com/auth/oauth2/corp/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	if got := query.Get("scope"); got != "profile email" {
		t.Fatalf("scope = %q, want profile email", got)
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if got := query.Get("code_challenge"); got == "" {
		t.Fatal("authorization URL carries no PKCE challenge")
	}
	if got := query.Get("state"); len(got) < 22 {
		t.Fatalf("state = %q, want at least 128 bits of entropy", got)
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
		})
	}
}

func TestStartFailsClosedWhenTheProviderIsNotInitialized(t *testing.T) {
	harness := newHarness(t, nil)

	// A registry with a pass-through validator can hold the zero Provider, which
	// is the state a provider reaches when it was never built by oauth2.New.
	registry := federationcore.NewRegistry[*oauth2core.Provider](func(string, *oauth2core.Provider) error { return nil })
	if err := registry.Put(testProvider, &oauth2core.Provider{}); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}
	adapter, err := New(DefaultConfig(), registry, harness.login, WithLogger(harness.logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, recorder := newStartCtx(t, testProvider, "")
	if err := adapter.start(context.Background(), ctx); err != nil {
		t.Fatalf("start handler error = %v", err)
	}
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if values := recorder.Header().Values("Set-Cookie"); len(values) != 0 {
		t.Fatalf("failed start installed cookies: %q", values)
	}
}

// recordingLogin records the calls the adapter makes. A start that must fail
// closed is observed through it, because the real base's own validation would
// otherwise mask an adapter that kept going after Begin failed.
type recordingLogin struct {
	stubLogin
	mu    sync.Mutex
	begun []federationweb.BeginRequest
	fails []federationweb.ErrorCode
}

func (l *recordingLogin) Begin(_ context.Context, _ *transportweb.Ctx, request federationweb.BeginRequest) (federationweb.Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.begun = append(l.begun, request)
	return federationweb.Transaction{}, nil
}

func (l *recordingLogin) Fail(_ *transportweb.Ctx, code federationweb.ErrorCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails = append(l.fails, code)
}

func (l *recordingLogin) recorded() ([]federationweb.BeginRequest, []federationweb.ErrorCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]federationweb.BeginRequest(nil), l.begun...), append([]federationweb.ErrorCode(nil), l.fails...)
}

// TestStartFailureDoesNotStoreATransaction pins the adapter's own contract: a
// neutral client that cannot begin a login is reported as federation_failed
// before the shared base is asked to store anything. The zero Provider can be
// stored only through a pass-through registry validator, which is exactly the
// state an un-built provider reaches.
func TestStartFailureDoesNotStoreATransaction(t *testing.T) {
	registry := federationcore.NewRegistry[*oauth2core.Provider](func(string, *oauth2core.Provider) error { return nil })
	if err := registry.Put(testProvider, &oauth2core.Provider{}); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}
	login := &recordingLogin{}
	adapter, err := New(DefaultConfig(), registry, login, WithLogger(&capturingLogger{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, recorder := newStartCtx(t, testProvider, "")
	if err := adapter.start(context.Background(), ctx); err != nil {
		t.Fatalf("start handler error = %v", err)
	}
	begun, fails := login.recorded()
	if len(begun) != 0 {
		t.Fatalf("start stored %v after Begin failed", begun)
	}
	if len(fails) != 1 || fails[0] != federationweb.CodeFailed {
		t.Fatalf("Fail calls = %v, want one %q", fails, federationweb.CodeFailed)
	}
	if values := recorder.Header().Values("Set-Cookie"); len(values) != 0 {
		t.Fatalf("failed start installed cookies: %q", values)
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
	if location := locationOf(recorder); strings.HasPrefix(location, "http") {
		t.Fatalf("failed start redirected to the IdP: %q", location)
	}
}

func TestCallbackCompletesTheLoginWithAFreshSession(t *testing.T) {
	harness := newHarness(t, nil)
	loginTime := harness.clock.Now()

	authorization, binding := harness.start(t, "/dashboard")
	ctx, recorder := harness.callback(t, authorization, binding, "", &http.Cookie{Name: testSessionCookie, Value: "old-session-id"})

	if status := statusOf(ctx); status != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", status)
	}
	if location := locationOf(recorder); location != "/dashboard" {
		t.Fatalf("callback location = %q, want /dashboard", location)
	}
	if cookies := recorder.Header().Values("Cache-Control"); len(cookies) == 0 || cookies[0] != "no-store" {
		t.Fatalf("callback Cache-Control = %q, want no-store", cookies)
	}

	sessionCookie := responseCookie(t, recorder, testSessionCookie)
	if sessionCookie.Value != "new-session-id" {
		t.Fatalf("session cookie = %q, want the newly created session", sessionCookie.Value)
	}

	operations := harness.sessions.operations()
	if len(operations) < 3 {
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
	if got := created.Attributes["idp_protocol"]; got != "oauth2" {
		t.Fatalf("idp_protocol attribute = %v, want oauth2", got)
	}
	if got := created.Attributes["idp"]; got != testProvider {
		t.Fatalf("idp attribute = %v, want %q", got, testProvider)
	}
	if got := created.Attributes["role"]; got != "admin" {
		t.Fatalf("application attribute role = %v, want admin", got)
	}

	identities := harness.resolver.identities()
	if len(identities) != 1 {
		t.Fatalf("resolver saw %d identities, want 1", len(identities))
	}
	identity := identities[0]
	if identity.Protocol != "oauth2" || identity.Provider != testProvider {
		t.Fatalf("identity protocol/provider = %q/%q", identity.Protocol, identity.Provider)
	}
	if identity.Subject != testSubject {
		t.Fatalf("identity subject = %q, want %q", identity.Subject, testSubject)
	}
	if identity.Issuer != harness.idp.issuer {
		t.Fatalf("identity issuer = %q, want %q", identity.Issuer, harness.idp.issuer)
	}
	if got := identity.Claims["email"]; got != "alice@example.com" {
		t.Fatalf("identity email claim = %v", got)
	}
}

func TestCallbackRejectsAReplayedTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	if _, recorder := harness.callback(t, authorization, binding, ""); locationOf(recorder) != "/dashboard" {
		t.Fatalf("first callback location = %q, want /dashboard", locationOf(recorder))
	}

	_, recorder := harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("replay failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := harness.idp.tokenCount(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1: a replay must not reach the token endpoint", got)
	}
	if operations := harness.sessions.operations(); countOperations(operations, "create") != 1 {
		t.Fatalf("session operations = %v, want exactly one create", operations)
	}
}

func TestCallbackRejectsAnotherBrowser(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	_, recorder := harness.callback(t, authorization, &http.Cookie{Name: testBindingCookie, Value: "another-browser-secret"}, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("other-browser failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := harness.idp.tokenCount(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}

	// The mismatching callback destroyed the transaction, so the browser that
	// really started the login cannot complete it later either.
	if _, recorder := harness.callback(t, authorization, binding, ""); failureCodeOf(t, recorder) != string(federationweb.CodeBadRequest) {
		t.Fatal("the transaction survived a cross-browser callback")
	}
}

func TestCallbackRejectsAnExpiredTransaction(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	harness.clock.Advance(5*time.Minute + time.Second)

	_, recorder := harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("expired failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := harness.idp.tokenCount(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}
}

func TestCallbackMapsProviderErrorResponsesToFixedCodes(t *testing.T) {
	harness := newHarness(t, nil)

	for name, testCase := range map[string]struct {
		idpError string
		want     federationweb.ErrorCode
	}{
		"access denied":  {idpError: "access_denied", want: federationweb.CodeDenied},
		"server error":   {idpError: "temporarily_unavailable", want: federationweb.CodeFailed},
		"malicious text": {idpError: "<script>alert(1)</script>", want: federationweb.CodeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			authorization, binding := harness.start(t, "/dashboard")
			state := authorization.Query().Get("state")
			_, recorder := harness.callback(t, authorization, binding, "state="+url.QueryEscape(state)+"&error="+url.QueryEscape(testCase.idpError))
			if got := failureCodeOf(t, recorder); got != string(testCase.want) {
				t.Fatalf("failure code = %q, want %q", got, testCase.want)
			}
			if location := locationOf(recorder); strings.Contains(location, "script") || strings.Contains(location, testCase.idpError) {
				t.Fatalf("failure location echoes IdP text: %q", location)
			}
		})
	}

	if got := harness.idp.tokenCount(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0: an IdP error must not be exchanged", got)
	}
	if text := harness.logger.text(); strings.Contains(text, "<script>") {
		t.Fatalf("log carries the raw IdP error value: %q", text)
	}
}

// TestCallbackBurnsTheTransactionOnAProviderError pins the ordering that makes
// the callback single-use even when the IdP reports the failure: the
// transaction is consumed before the error parameter is interpreted, so the
// same state cannot be presented again with an authorization code.
func TestCallbackBurnsTheTransactionOnAProviderError(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	state := authorization.Query().Get("state")
	_, recorder := harness.callback(t, authorization, binding, "state="+url.QueryEscape(state)+"&error=access_denied")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeDenied) {
		t.Fatalf("error callback code = %q, want %q", got, federationweb.CodeDenied)
	}

	_, recorder = harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("the transaction survived an IdP error callback: second code = %q", got)
	}
	if got := harness.idp.tokenCount(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}
}

func TestCallbackRejectsAMissingAuthorizationCode(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	_, recorder := harness.callback(t, authorization, binding, "state="+url.QueryEscape(authorization.Query().Get("state")))
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeBadRequest) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeBadRequest)
	}
	if got := harness.idp.tokenCount(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}
}

// TestCallbackReportsExchangeFailuresAsFailed drives every failure the neutral
// client can report through a real HTTP exchange: a rejected code, a non-Bearer
// token, a userinfo error status, a redirect the client refuses, an oversized
// body, and a document that is not a JSON object. Each one must end as
// federation_failed with no session and no provider detail in the log or the
// failure location.
func TestCallbackReportsExchangeFailuresAsFailed(t *testing.T) {
	const marker = "provider-detail-marker"

	for name, testCase := range map[string]struct {
		mutateProvider    func(*oauth2core.Config)
		configure         func(*fakeIDP)
		wantTokenCalls    int
		wantUserInfoCalls int
	}{
		"token endpoint rejects the code": {
			configure: func(idp *fakeIDP) {
				idp.setTokenResponse(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"`+marker+`"}`)
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 0,
		},
		"non-Bearer token type": {
			configure: func(idp *fakeIDP) {
				idp.setTokenResponse(http.StatusOK, `{"token_type":"mac","access_token":"`+testAccessToken+`"}`)
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 0,
		},
		"userinfo endpoint error": {
			configure: func(idp *fakeIDP) {
				idp.setUserInfoResponse(http.StatusInternalServerError, marker)
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 1,
		},
		"userinfo redirect": {
			configure: func(idp *fakeIDP) {
				idp.setUserInfoRedirect()
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 1,
		},
		"oversized userinfo body": {
			mutateProvider: func(config *oauth2core.Config) {
				config.Policy.MaxBodyBytes = 128
			},
			configure: func(idp *fakeIDP) {
				idp.setUserInfoResponse(http.StatusOK, `{"sub":"`+testSubject+`","pad":"`+strings.Repeat(marker, 8)+`"}`)
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 1,
		},
		"non-object userinfo document": {
			configure: func(idp *fakeIDP) {
				idp.setUserInfoResponse(http.StatusOK, `["`+marker+`"]`)
			},
			wantTokenCalls:    1,
			wantUserInfoCalls: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newHarness(t, func(options *harnessOptions) {
				options.mutateProvider = testCase.mutateProvider
			})
			testCase.configure(harness.idp)

			authorization, binding := harness.start(t, "/dashboard")
			state := authorization.Query().Get("state")
			_, recorder := harness.callback(t, authorization, binding, "")

			if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
				t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
			}
			if location := locationOf(recorder); strings.Contains(location, marker) || strings.Contains(location, "invalid_grant") {
				t.Fatalf("failure location echoes provider text: %q", location)
			}
			if text := harness.logger.text(); strings.Contains(text, marker) {
				t.Fatalf("log carries provider response text: %q", text)
			}
			if strings.Contains(harness.logger.text(), state) {
				t.Fatalf("log carries the transaction state: %q", harness.logger.text())
			}
			if got := harness.idp.tokenCount(); got != testCase.wantTokenCalls {
				t.Fatalf("token endpoint calls = %d, want %d", got, testCase.wantTokenCalls)
			}
			if got := harness.idp.userInfoCount(); got != testCase.wantUserInfoCalls {
				t.Fatalf("userinfo endpoint calls = %d, want %d", got, testCase.wantUserInfoCalls)
			}
			if got := harness.idp.redirectCount(); got != 0 {
				t.Fatalf("the neutral client followed %d userinfo redirects, want 0", got)
			}
			if operations := harness.sessions.operations(); countOperations(operations, "create") != 0 {
				t.Fatalf("session operations = %v, want no session for a failed exchange", operations)
			}
		})
	}
}

func TestCallbackTreatsAnEmptyLocalSubjectAsAFailure(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.emptySubject = true
	})

	authorization, binding := harness.start(t, "/dashboard")
	_, recorder := harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("session operations = %v, want none for an invalid subject", operations)
	}
}

func TestCallbackRejectsApplicationAttributesThatOverrideReservedKeys(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.attributes = map[string]any{"idp_protocol": "attacker-controlled"}
	})

	authorization, binding := harness.start(t, "/dashboard")
	_, recorder := harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeFailed) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeFailed)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("session operations = %v, want none when reserved keys are overridden", operations)
	}
}

func TestCallbackRendersResolverRefusalsAsDenied(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.refused = true
	})

	authorization, binding := harness.start(t, "/dashboard")
	_, recorder := harness.callback(t, authorization, binding, "")
	if got := failureCodeOf(t, recorder); got != string(federationweb.CodeDenied) {
		t.Fatalf("failure code = %q, want %q", got, federationweb.CodeDenied)
	}
	if operations := harness.sessions.operations(); len(operations) != 0 {
		t.Fatalf("session operations = %v, want none for a refusal", operations)
	}
	if values := recorder.Header().Values("Set-Cookie"); len(values) != 1 {
		// Consume clears the binding cookie; a refusal installs no session.
		t.Fatalf("refusal Set-Cookie = %q, want only the binding-cookie removal", values)
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
			authorization, binding := harness.start(t, returnTo)
			_, recorder := harness.callback(t, authorization, binding, "")
			if location := locationOf(recorder); location != "/" {
				t.Fatalf("hostile return_to landed on %q, want the default /", location)
			}
		})
	}

	authorization, binding := harness.start(t, "https://app.example.com/dashboard")
	_, recorder := harness.callback(t, authorization, binding, "")
	if location := locationOf(recorder); location != "https://app.example.com/dashboard" {
		t.Fatalf("allowlisted absolute return_to landed on %q", location)
	}
}

func TestLoginNeverLogsTransactionOrTokenMaterial(t *testing.T) {
	harness := newHarness(t, nil)

	authorization, binding := harness.start(t, "/dashboard")
	state := authorization.Query().Get("state")
	if _, recorder := harness.callback(t, authorization, binding, ""); locationOf(recorder) != "/dashboard" {
		t.Fatalf("callback location = %q", locationOf(recorder))
	}

	text := harness.logger.text()
	for name, secret := range map[string]string{
		"state":          state,
		"binding secret": binding.Value,
		"client secret":  testClientSecret,
		"authorization":  testAuthorization,
		"access token":   testAccessToken,
	} {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("log carries the %s: %q", name, text)
		}
	}
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
