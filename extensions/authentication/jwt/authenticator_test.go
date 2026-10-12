package jwt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/xbcio/xbc/extensions/authentication"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return configuredPlugin(t, nil)
}

func TestPluginAuthenticateAcceptsValidTokenAndPublishesPrincipal(t *testing.T) {
	p := newTestPlugin(t)
	token, err := p.Sign("alice", Claims{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := authentication.Presented(token).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Authenticated() {
		t.Fatal("Authenticated() = false, want true")
	}
	principal, ok := result.Principal()
	if !ok {
		t.Fatal("Principal() ok = false")
	}
	typed, ok := principal.(authentication.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want authentication.Principal", principal)
	}
	if typed.Subject != "alice" || typed.AuthMethod != "jwt" || typed.Attributes["role"] != "admin" {
		t.Fatalf("principal = %#v", typed)
	}
}

func TestPluginAuthenticateRejectsInvalidCredentialType(t *testing.T) {
	p := newTestPlugin(t)
	credential, ok := authentication.Presented(42).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	if challenges := result.Challenges(); len(challenges) != 1 || challenges[0] != authentication.Challenge("Bearer") {
		t.Fatalf("Challenges() = %v, want [Bearer]", challenges)
	}
}

func signTestToken(t *testing.T, method jwtlib.SigningMethod, claims jwtlib.MapClaims) string {
	t.Helper()
	raw, err := jwtlib.NewWithClaims(method, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return raw
}

func TestPluginAuthenticateRejectsInvalidTokens(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Algorithms = []string{"HS256"}
	})

	expired := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(-time.Minute)),
	})
	missingExp := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{"sub": "alice"})
	wrongAlgorithm := signTestToken(t, jwtlib.SigningMethodHS512, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	noneToken := jwtlib.NewWithClaims(jwtlib.SigningMethodNone, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	none, err := noneToken.SignedString(jwtlib.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaToken := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	rsaSigned, err := rsaToken.SignedString(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	noSubject := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})

	tests := []struct {
		name  string
		token string
	}{
		{name: "expired", token: expired},
		{name: "missing exp", token: missingExp},
		{name: "wrong algorithm", token: wrongAlgorithm},
		{name: "none algorithm", token: none},
		{name: "asymmetric algorithm confusion", token: rsaSigned},
		{name: "structurally malformed", token: "not-a-token"},
		{name: "missing subject", token: noSubject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credential, ok := authentication.Presented(tt.token).Credential()
			if !ok {
				t.Fatal("Credential() ok = false")
			}

			result, err := p.Authenticate(context.Background(), credential)
			if err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}
			if !result.Rejected() {
				t.Fatal("Rejected() = false, want true")
			}
			kind, ok := result.Rejection()
			if !ok || kind != authentication.RejectionInvalidCredential {
				t.Fatalf("Rejection() = %v, ok=%v, want RejectionInvalidCredential", kind, ok)
			}
			challenges := result.Challenges()
			if len(challenges) != 1 || challenges[0] != authentication.Challenge("Bearer") {
				t.Fatalf("Challenges() = %v, want [Bearer]", challenges)
			}
			reason, ok := result.Reason()
			if !ok {
				t.Fatal("Reason() ok = false")
			}
			// Deliberately do not leak signature, expiry, issuer, or parser
			// detail: the reason is a fixed, generic string regardless of why
			// verification failed, including the missing-subject case.
			const wantReason = "invalid token"
			if string(reason) != wantReason {
				t.Fatalf("reason = %q, want %q", reason, wantReason)
			}
			// The equality check above subsumes this, but it documents the
			// intent explicitly for the next reader.
			if strings.Contains(string(reason), tt.token) {
				t.Fatalf("reason leaked token material: %q", reason)
			}
		})
	}
}
