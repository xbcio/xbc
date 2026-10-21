package oidc

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestOperationsRequireDiscovery(t *testing.T) {
	idp := newFakeIDP(t)
	provider, err := New(idp.providerConfig())
	requireNoError(t, err, "New")

	_, err = provider.Begin()
	requireErrorIs(t, err, ErrNotDiscovered, "Begin before Discover")

	_, err = provider.Exchange(context.Background(), "auth-code", AuthorizationRequest{
		Nonce:        strings.Repeat("n", 43),
		CodeVerifier: strings.Repeat("v", 43),
	})
	requireErrorIs(t, err, ErrNotDiscovered, "Exchange before Discover")

	jwksHits, tokenHits := idp.counts()
	if jwksHits != 0 || tokenHits != 0 {
		t.Fatalf("undiscovered provider made outbound calls: jwks=%d token=%d", jwksHits, tokenHits)
	}
}

func TestDiscoverRejectsIssuerMismatch(t *testing.T) {
	idp := newFakeIDP(t)
	idp.updateDiscovery(func(f *fakeIDP) {
		f.discoveryMutate = func(document map[string]any) {
			document["issuer"] = "https://attacker.example.com"
		}
	})
	provider, err := New(idp.providerConfig())
	requireNoError(t, err, "New")
	err = provider.Discover(context.Background())
	requireErrorIs(t, err, ErrDiscovery, "Discover with mismatched issuer")

	if _, err := provider.Begin(); err == nil {
		t.Fatal("Begin succeeded after a failed Discover")
	}
}

func TestDiscoverRejectsUnusableEndpoints(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(document map[string]any)
	}{
		{"missing authorization endpoint", func(document map[string]any) { delete(document, "authorization_endpoint") }},
		{"missing token endpoint", func(document map[string]any) { delete(document, "token_endpoint") }},
		{"missing jwks uri", func(document map[string]any) { delete(document, "jwks_uri") }},
		{"relative authorization endpoint", func(document map[string]any) { document["authorization_endpoint"] = "/authorize" }},
		{"non-HTTP token endpoint", func(document map[string]any) {
			document["token_endpoint"] = strings.Replace(document["token_endpoint"].(string), "http://", "ftp://", 1)
		}},
		{"non-HTTP jwks uri", func(document map[string]any) { document["jwks_uri"] = "file:///tmp/jwks.json" }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.updateDiscovery(func(f *fakeIDP) { f.discoveryMutate = testCase.mutate })
			provider, err := New(idp.providerConfig())
			requireNoError(t, err, "New")
			err = provider.Discover(context.Background())
			requireErrorIs(t, err, ErrDiscovery, testCase.name)
		})
	}
}

func TestDiscoveredEndpointsObeyTheOutboundPolicy(t *testing.T) {
	provider, err := New(validConfig())
	requireNoError(t, err, "New")

	if _, err := provider.discoveredEndpoint("token_endpoint", "https://idp.example.com/token"); err != nil {
		t.Fatalf("an HTTPS endpoint was rejected: %v", err)
	}
	_, err = provider.discoveredEndpoint("token_endpoint", "http://idp.example.com/token")
	requireErrorIs(t, err, ErrDiscovery, "plain HTTP endpoint without the development switch")
}

func TestBeginBuildsAuthorizationURL(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	transaction, err := provider.Begin()
	requireNoError(t, err, "Begin")

	parsed, err := url.Parse(transaction.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if got, want := parsed.Scheme+"://"+parsed.Host+parsed.Path, idp.issuer+"/authorize"; got != want {
		t.Fatalf("authorization endpoint = %q, want %q", got, want)
	}
	query := parsed.Query()
	expected := map[string]string{
		"response_type":         "code",
		"client_id":             idp.clientID,
		"redirect_uri":          idp.redirectURI,
		"scope":                 "openid profile email",
		"code_challenge_method": "S256",
		"code_challenge":        codeChallenge(transaction.CodeVerifier),
		"state":                 transaction.State,
		"nonce":                 transaction.Nonce,
	}
	for name, want := range expected {
		if got := query.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if !validCodeVerifier(transaction.CodeVerifier) {
		t.Fatalf("code verifier %q is not a valid PKCE verifier", transaction.CodeVerifier)
	}
	if len(transaction.State) < 43 || len(transaction.Nonce) < 43 {
		t.Fatalf("transaction entropy is too small: state=%d nonce=%d characters", len(transaction.State), len(transaction.Nonce))
	}

	second, err := provider.Begin()
	requireNoError(t, err, "second Begin")
	if second.State == transaction.State || second.Nonce == transaction.Nonce || second.CodeVerifier == transaction.CodeVerifier {
		t.Fatal("two transactions reused the same state, nonce, or verifier")
	}
}

func TestExchangeRejectsIncompleteTransaction(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	validVerifier := strings.Repeat("v", 43)

	testCases := []struct {
		name        string
		code        string
		transaction AuthorizationRequest
	}{
		{"missing nonce", "auth-code", AuthorizationRequest{CodeVerifier: validVerifier}},
		{"missing code verifier", "auth-code", AuthorizationRequest{Nonce: strings.Repeat("n", 43)}},
		{"short code verifier", "auth-code", AuthorizationRequest{Nonce: strings.Repeat("n", 43), CodeVerifier: "short"}},
		{"code verifier with invalid characters", "auth-code", AuthorizationRequest{
			Nonce:        strings.Repeat("n", 43),
			CodeVerifier: strings.Repeat("v", 42) + "!",
		}},
		{"missing code", "", AuthorizationRequest{Nonce: strings.Repeat("n", 43), CodeVerifier: validVerifier}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := provider.Exchange(context.Background(), testCase.code, testCase.transaction)
			requireErrorIs(t, err, ErrInvalidTransaction, testCase.name)
		})
	}
	if _, tokenHits := idp.counts(); tokenHits != 0 {
		t.Fatalf("an invalid transaction reached the token endpoint %d times", tokenHits)
	}
}

func TestNilContextAndNilProviderAreExplicitErrors(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)

	//nolint:staticcheck // a nil context is exactly the misuse under test.
	requireErrorIs(t, provider.Discover(nil), ErrNilContext, "Discover with nil context")
	//nolint:staticcheck // a nil context is exactly the misuse under test.
	_, err := provider.Exchange(nil, "auth-code", AuthorizationRequest{})
	requireErrorIs(t, err, ErrNilContext, "Exchange with nil context")

	var missing *Provider
	if _, err := missing.Begin(); err == nil {
		t.Fatal("Begin on a nil provider succeeded")
	}
	if missing.Name() != "" {
		t.Fatalf("Name on a nil provider = %q", missing.Name())
	}
}

func TestProviderRendersWithTheClientSecretRedacted(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	const secret = "client-secret"

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, provider)
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s rendered the client secret: %s", format, rendered)
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("%s did not mark the client secret as redacted: %s", format, rendered)
		}
	}
}

func TestNilProviderRendersWithoutPanicking(t *testing.T) {
	var provider *Provider
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, provider)
		if rendered != "<nil>" {
			t.Fatalf("%s on a nil provider rendered %q, want <nil>", format, rendered)
		}
	}
}
