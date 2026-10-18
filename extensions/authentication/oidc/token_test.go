package oidc

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

func TestTokenEndpointClientSecretBasicIsTheDefault(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	identity, err := idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")
	if identity.Subject != "idp-subject" {
		t.Fatalf("subject = %q, want idp-subject", identity.Subject)
	}

	form, authorization, tokenURL, method := idp.lastToken()
	if method != "POST" {
		t.Fatalf("token request method = %q, want POST", method)
	}
	if !strings.HasPrefix(authorization, "Basic ") {
		t.Fatalf("Authorization header = %q, want Basic credentials", authorization)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, "Basic "))
	requireNoError(t, err, "decode Basic credentials")
	want := url.QueryEscape(idp.clientID) + ":" + url.QueryEscape("client-secret")
	if string(decoded) != want {
		t.Fatalf("Basic credentials = %q, want %q", decoded, want)
	}
	if form.Get("client_secret") != "" || form.Get("client_id") != "" {
		t.Fatal("client credentials appeared in the request body under client_secret_basic")
	}
	if form.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q, want authorization_code", form.Get("grant_type"))
	}
	if form.Get("redirect_uri") != idp.redirectURI {
		t.Fatalf("redirect_uri = %q, want %q", form.Get("redirect_uri"), idp.redirectURI)
	}
	if form.Get("code_verifier") == "" {
		t.Fatal("token request carried no PKCE code verifier")
	}
	if strings.Contains(tokenURL, "client_secret") || strings.Contains(tokenURL, "code_verifier") {
		t.Fatalf("token request URL carried credentials or the verifier: %s", tokenURL)
	}
	if strings.Contains(tokenURL, "?") {
		t.Fatalf("token request URL carries a query: %s", tokenURL)
	}
}

func TestTokenEndpointClientSecretPost(t *testing.T) {
	idp := newFakeIDP(t)
	config := idp.providerConfig()
	config.TokenEndpointAuthMethod = ClientSecretPost
	provider, err := New(config)
	requireNoError(t, err, "New")
	requireNoError(t, provider.Discover(t.Context()), "Discover")

	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")

	form, authorization, _, _ := idp.lastToken()
	if authorization != "" {
		t.Fatalf("Authorization header = %q, want none under client_secret_post", authorization)
	}
	if form.Get("client_id") != idp.clientID {
		t.Fatalf("client_id = %q, want %q", form.Get("client_id"), idp.clientID)
	}
	if form.Get("client_secret") != "client-secret" {
		t.Fatalf("client_secret in body = %q, want the configured secret", form.Get("client_secret"))
	}
}

func TestTokenResponseValidation(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*fakeIDP)
	}{
		{"non-Bearer token type", func(idp *fakeIDP) { idp.updateToken(func(f *fakeIDP) { f.tokenType = "mac" }) }},
		{"missing id token", func(idp *fakeIDP) { idp.updateToken(func(f *fakeIDP) { f.omitIDToken = true }) }},
		{"missing access token", func(idp *fakeIDP) { idp.updateToken(func(f *fakeIDP) { f.omitAccessToken = true }) }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			provider := idp.newProvider(t)
			testCase.mutate(idp)
			_, err := idp.exchange(t, provider)
			requireErrorIs(t, err, ErrInvalidTokenResponse, testCase.name)
		})
	}
}

func TestTokenEndpointErrorBodyIsNeverEchoed(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	const marker = "idp-response-marker"
	idp.setTokenResponse(400, `{"error":"invalid_grant","error_description":"`+marker+` client-secret access-token"}`)

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrTokenExchange, "failed token exchange")
	for _, secret := range []string{marker, "client-secret", "access-token", "invalid_grant"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("token error echoed %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("token error does not report the status: %v", err)
	}
}

func TestEmptySecretNeverReachesTheTokenEndpoint(t *testing.T) {
	// A configuration without a secret is rejected statically, so the token
	// endpoint can never observe an empty credential.
	config := validConfig()
	config.ClientSecret = outbound.Secret("")
	_, err := New(config)
	requireErrorIs(t, err, ErrInvalidConfig, "missing client secret")
}
