package oauth2

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

func TestBeginBuildsAPKCEAuthorizationURL(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)

	transaction, err := provider.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(transaction.URL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if got := parsed.Path; got != "/authorize" {
		t.Fatalf("authorization path = %q, want /authorize", got)
	}
	query := parsed.Query()
	if got := query.Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if got := query.Get("client_id"); got != testClientID {
		t.Fatalf("client_id = %q, want %q", got, testClientID)
	}
	if got := query.Get("redirect_uri"); got != testRedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", got, testRedirectURI)
	}
	if got := query.Get("scope"); got != "profile email" {
		t.Fatalf("scope = %q, want %q", got, "profile email")
	}
	if got := query.Get("state"); len(got) < 43 {
		t.Fatalf("state = %q, want at least 256 bits of entropy", got)
	}
	if got := transaction.State; got != query.Get("state") {
		t.Fatalf("Transaction.State = %q, URL carries %q", got, query.Get("state"))
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if got := query.Get("code_challenge"); got != codeChallenge(transaction.CodeVerifier) {
		t.Fatalf("code_challenge = %q, want the S256 challenge of the verifier", got)
	}
	if query.Get("nonce") != "" {
		t.Fatalf("authorization URL carries a nonce: %q", query.Get("nonce"))
	}
	if strings.Contains(transaction.URL, testClientSecret) {
		t.Fatalf("authorization URL carries the client secret: %s", transaction.URL)
	}
}

func TestBeginPreservesTheConfiguredAuthorizationQuery(t *testing.T) {
	idp := newFakeIDP(t)
	config := idp.providerConfig()
	config.AuthorizationEndpoint = idp.issuer + "/authorize?tenant=corp"
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	transaction, err := provider.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(transaction.URL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if got := parsed.Query().Get("tenant"); got != "corp" {
		t.Fatalf("tenant = %q, want the configured value", got)
	}
	if got := parsed.Query().Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
}

func TestExchangeCompletesTheAuthorizationCodeFlow(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	transaction := idp.begin(t, provider)

	identity, err := provider.Exchange(context.Background(), testAuthorizationCode, transaction)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if identity.Protocol != "oauth2" {
		t.Fatalf("Protocol = %q, want oauth2", identity.Protocol)
	}
	if identity.Provider != testProvider {
		t.Fatalf("Provider = %q, want %q", identity.Provider, testProvider)
	}
	if identity.Issuer != idp.issuer {
		t.Fatalf("Issuer = %q, want %q", identity.Issuer, idp.issuer)
	}
	if identity.Subject != testSubject {
		t.Fatalf("Subject = %q, want %q", identity.Subject, testSubject)
	}
	if !identity.AuthTime.Equal(idp.clock.Now()) {
		t.Fatalf("AuthTime = %v, want the injected clock %v", identity.AuthTime, idp.clock.Now())
	}
	if got := identity.Claims["email"]; got != "alice@example.com" {
		t.Fatalf("email claim = %v", got)
	}
	if got := identity.Claims["name"]; got != "Alice" {
		t.Fatalf("name claim = %v", got)
	}

	form, authorization, method, requestURL := idp.lastToken()
	if method != "POST" {
		t.Fatalf("token method = %q, want POST", method)
	}
	if parsed, err := url.Parse(requestURL); err != nil || parsed.RawQuery != "" {
		t.Fatalf("token request URL = %q, want no query", requestURL)
	}
	if got := form.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := form.Get("code"); got != testAuthorizationCode {
		t.Fatalf("code = %q", got)
	}
	if got := form.Get("redirect_uri"); got != testRedirectURI {
		t.Fatalf("redirect_uri = %q", got)
	}
	if got := form.Get("code_verifier"); got != transaction.CodeVerifier {
		t.Fatalf("code_verifier = %q, want the transaction verifier", got)
	}
	if !strings.HasPrefix(authorization, "Basic ") {
		t.Fatalf("token Authorization = %q, want client_secret_basic by default", authorization)
	}
	if form.Get("client_secret") != "" || form.Get("client_id") != "" {
		t.Fatalf("basic authentication also put credentials in the body: %v", form)
	}

	userInfoAuthorization, userInfoURL := idp.lastUserInfo()
	if userInfoAuthorization != "Bearer "+testAccessToken {
		t.Fatalf("userinfo Authorization = %q, want the bearer access token", userInfoAuthorization)
	}
	if parsed, err := url.Parse(userInfoURL); err != nil || parsed.RawQuery != "" {
		t.Fatalf("userinfo request URL = %q, want the registered endpoint without a query", userInfoURL)
	}

	if token, userInfo, _ := idp.counts(); token != 1 || userInfo != 1 {
		t.Fatalf("token/userinfo calls = %d/%d, want 1/1", token, userInfo)
	}
}

func TestExchangeRejectsAnInvalidTransaction(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	transaction := idp.begin(t, provider)

	for name, testCase := range map[string]struct {
		ctx       context.Context
		code      string
		verifier  string
		wantError error
	}{
		"nil context":         {ctx: nil, code: testAuthorizationCode, verifier: transaction.CodeVerifier, wantError: ErrNilContext},
		"empty code":          {ctx: context.Background(), code: "", verifier: transaction.CodeVerifier, wantError: ErrInvalidTransaction},
		"overlong code":       {ctx: context.Background(), code: strings.Repeat("a", maxAuthorizationCodeLength+1), verifier: transaction.CodeVerifier, wantError: ErrInvalidTransaction},
		"empty verifier":      {ctx: context.Background(), code: testAuthorizationCode, verifier: "", wantError: ErrInvalidTransaction},
		"short verifier":      {ctx: context.Background(), code: testAuthorizationCode, verifier: "short", wantError: ErrInvalidTransaction},
		"overlong verifier":   {ctx: context.Background(), code: testAuthorizationCode, verifier: strings.Repeat("a", 129), wantError: ErrInvalidTransaction},
		"verifier with space": {ctx: context.Background(), code: testAuthorizationCode, verifier: strings.Repeat("a", 42) + " ", wantError: ErrInvalidTransaction},
	} {
		t.Run(name, func(t *testing.T) {
			request := AuthorizationRequest{State: transaction.State, CodeVerifier: testCase.verifier}
			_, err := provider.Exchange(testCase.ctx, testCase.code, request)
			requireErrorIs(t, err, testCase.wantError, "Exchange")
		})
	}

	if token, userInfo, _ := idp.counts(); token != 0 || userInfo != 0 {
		t.Fatalf("invalid transactions reached the network: token/userinfo = %d/%d", token, userInfo)
	}
}

func TestExchangeRejectsANonBearerTokenType(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setToken(func(idp *fakeIDP) { idp.tokenType = "mac" })

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidTokenResponse, "Exchange")
	if _, userInfo, _ := idp.counts(); userInfo != 0 {
		t.Fatalf("userinfo calls = %d, want 0 for a non-Bearer token", userInfo)
	}
}

func TestExchangeRejectsATokenResponseWithoutAccessToken(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setToken(func(idp *fakeIDP) { idp.omitAccessToken = true })

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidTokenResponse, "Exchange")
	if _, userInfo, _ := idp.counts(); userInfo != 0 {
		t.Fatalf("userinfo calls = %d, want 0 without an access token", userInfo)
	}
}

func TestExchangeRejectsANonJSONTokenResponse(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setToken(func(idp *fakeIDP) { idp.tokenRawBody = "not json" })

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidTokenResponse, "Exchange")
}

func TestTokenEndpointFailureReportsOnlyTheStatus(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setToken(func(idp *fakeIDP) {
		idp.tokenStatus = 400
		idp.tokenRawBody = `{"error":"invalid_grant","error_description":"raw provider text"}`
	})

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrTokenExchange, "Exchange")
	text := err.Error()
	if !strings.Contains(text, "status 400") {
		t.Fatalf("error = %q, want the status code", text)
	}
	for name, secret := range map[string]string{
		"provider body": `raw provider text`,
		"client secret": testClientSecret,
		"access token":  testAccessToken,
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("error carries the %s: %q", name, text)
		}
	}
	if _, userInfo, _ := idp.counts(); userInfo != 0 {
		t.Fatalf("userinfo calls = %d, want 0 after a failed exchange", userInfo)
	}
}

func TestTokenEndpointClientAuthenticationMethods(t *testing.T) {
	for name, testCase := range map[string]struct {
		method         TokenEndpointAuthMethod
		wantBasic      bool
		wantBodyID     bool
		wantBodySecret bool
	}{
		"basic": {method: ClientSecretBasic, wantBasic: true},
		"post":  {method: ClientSecretPost, wantBodyID: true, wantBodySecret: true},
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			config := idp.providerConfig()
			config.TokenEndpointAuthMethod = testCase.method
			provider, err := New(config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = idp.exchange(t, provider)
			requireNoError(t, err, "Exchange")

			form, authorization, _, _ := idp.lastToken()
			if testCase.wantBasic && !strings.HasPrefix(authorization, "Basic ") {
				t.Fatalf("Authorization = %q, want Basic credentials", authorization)
			}
			if !testCase.wantBasic && authorization != "" {
				t.Fatalf("Authorization = %q, want no header with client_secret_post", authorization)
			}
			if got := form.Get("client_id") != ""; got != testCase.wantBodyID {
				t.Fatalf("body client_id present = %v, want %v", got, testCase.wantBodyID)
			}
			if got := form.Get("client_secret") != ""; got != testCase.wantBodySecret {
				t.Fatalf("body client_secret present = %v, want %v", got, testCase.wantBodySecret)
			}
		})
	}
}

// TestTokenEndpointBasicCredentialsAreFormEncoded pins RFC 6749 section 2.3.1:
// the client identifier and secret are form-urlencoded before they are joined
// and base64-encoded. Real secrets routinely contain '+', '/', '=', '%', and
// ':', and skipping the encoding silently breaks every such IdP.
func TestTokenEndpointBasicCredentialsAreFormEncoded(t *testing.T) {
	idp := newFakeIDP(t)
	const (
		clientID     = "xbc+client/id=1%:"
		clientSecret = "s+e/c=r%t:1"
	)
	config := idp.providerConfig()
	config.ClientID = clientID
	config.ClientSecret = outbound.Secret(clientSecret)
	provider, err := New(config)
	requireNoError(t, err, "New")

	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")

	_, authorization, _, _ := idp.lastToken()
	const prefix = "Basic "
	if !strings.HasPrefix(authorization, prefix) {
		t.Fatalf("Authorization = %q, want Basic credentials", authorization)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, prefix))
	requireNoError(t, err, "decode Basic credentials")
	want := url.QueryEscape(clientID) + ":" + url.QueryEscape(clientSecret)
	if string(decoded) != want {
		t.Fatalf("Basic credentials = %q, want %q", decoded, want)
	}
}

func TestProviderIsImmutableAfterConstruction(t *testing.T) {
	idp := newFakeIDP(t)
	config := idp.providerConfig()
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	config.Scopes[0] = "changed"
	config.ClaimFields["email"] = "changed"
	config.Policy.AllowPrivate = false

	transaction, err := provider.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(transaction.URL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if got := parsed.Query().Get("scope"); got != "profile email" {
		t.Fatalf("scope = %q, want the original scopes", got)
	}

	identity, err := provider.Exchange(context.Background(), testAuthorizationCode, transaction)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := identity.Claims["email"]; got != "alice@example.com" {
		t.Fatalf("email claim = %v, want the original mapping", got)
	}
}

func TestNilProviderOperations(t *testing.T) {
	var provider *Provider
	if got := provider.Name(); got != "" {
		t.Fatalf("Name() on nil provider = %q", got)
	}
	if _, err := provider.Begin(); err == nil || !strings.Contains(err.Error(), ErrNilProvider.Error()) {
		t.Fatalf("Begin() on nil provider = %v, want ErrNilProvider", err)
	}
	if _, err := provider.Exchange(context.Background(), "code", AuthorizationRequest{CodeVerifier: strings.Repeat("a", 43)}); err == nil || !strings.Contains(err.Error(), ErrNilProvider.Error()) {
		t.Fatalf("Exchange() on nil provider = %v, want ErrNilProvider", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(format, provider); got != "<nil>" {
			t.Fatalf("%s on nil provider = %q, want <nil>", format, got)
		}
	}
}

func TestZeroProviderIsRejected(t *testing.T) {
	provider := &Provider{}
	if _, err := provider.Begin(); err == nil {
		t.Fatal("Begin() on a zero provider succeeded")
	}
	if err := ValidateProvider("corp", provider); err == nil {
		t.Fatal("ValidateProvider accepted a zero provider")
	}
}

func TestUserInfoRedirectIsNotFollowed(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setUserInfo(func(idp *fakeIDP) {
		idp.userInfoRedirect = idp.issuer + "/redirected"
	})

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUserInfo, "Exchange")
	if !strings.Contains(err.Error(), outbound.ErrTooManyRedirects.Error()) {
		t.Fatalf("error = %v, want the redirect rejection", err)
	}
	if _, _, redirect := idp.counts(); redirect != 0 {
		t.Fatalf("redirect target hits = %d, want 0", redirect)
	}
}
