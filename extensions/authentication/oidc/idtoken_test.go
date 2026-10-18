package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestExchangeReturnsVerifiedExternalIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	identity, err := idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")

	if identity.Protocol != "oidc" {
		t.Fatalf("Protocol = %q, want oidc", identity.Protocol)
	}
	if identity.Provider != "test" {
		t.Fatalf("Provider = %q, want test", identity.Provider)
	}
	if identity.Issuer != idp.issuer {
		t.Fatalf("Issuer = %q, want %q", identity.Issuer, idp.issuer)
	}
	if identity.Subject != "idp-subject" {
		t.Fatalf("Subject = %q, want idp-subject", identity.Subject)
	}
	if identity.Subject == identity.Claims["email"] {
		t.Fatal("Subject was taken from the email claim")
	}
	authTime := idp.clock.Now().Add(-5 * time.Minute)
	if !identity.AuthTime.Equal(authTime) {
		t.Fatalf("AuthTime = %s, want %s", identity.AuthTime, authTime)
	}
	if identity.Claims["email"] != "person@example.com" {
		t.Fatalf("Claims[email] = %v", identity.Claims["email"])
	}
	verified, ok := identity.Claims["email_verified"].(bool)
	if !ok || !verified {
		t.Fatalf("Claims[email_verified] = %v, want true", identity.Claims["email_verified"])
	}
}

func TestEmailVerifiedDefaultsFalseAndAuthTimeFallsBackToIssuedAt(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setClaimsBuilder(func(f *fakeIDP) map[string]any {
		claims := f.standardClaims()
		delete(claims, "email_verified")
		delete(claims, "auth_time")
		return claims
	})
	identity, err := idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")

	verified, ok := identity.Claims["email_verified"].(bool)
	if !ok || verified {
		t.Fatalf("Claims[email_verified] = %v, want false when absent", identity.Claims["email_verified"])
	}
	if !identity.AuthTime.Equal(idp.clock.Now()) {
		t.Fatalf("AuthTime = %s, want the iat instant %s", identity.AuthTime, idp.clock.Now())
	}
}

func TestES256TokenIsAccepted(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setSignKid("ec-1")
	_, err := idp.exchange(t, provider)
	requireNoError(t, err, "ES256 Exchange")
}

func TestIDTokenClaimValidation(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(idp *fakeIDP, claims map[string]any)
	}{
		{"issuer mismatch", func(_ *fakeIDP, claims map[string]any) {
			claims["iss"] = "https://other-idp.example.com"
		}},
		{"audience mismatch", func(_ *fakeIDP, claims map[string]any) {
			claims["aud"] = []string{"another-client"}
		}},
		{"multiple audiences without azp", func(_ *fakeIDP, claims map[string]any) {
			claims["aud"] = []string{"xbc-client", "another-client"}
		}},
		{"multiple audiences with the wrong azp", func(_ *fakeIDP, claims map[string]any) {
			claims["aud"] = []string{"xbc-client", "another-client"}
			claims["azp"] = "another-client"
		}},
		{"single audience with the wrong azp", func(_ *fakeIDP, claims map[string]any) {
			claims["azp"] = "another-client"
		}},
		{"expired beyond the leeway", func(idp *fakeIDP, claims map[string]any) {
			claims["exp"] = float64(idp.clock.Now().Add(-defaultLeeway - time.Second).Unix())
		}},
		{"not valid yet", func(idp *fakeIDP, claims map[string]any) {
			claims["nbf"] = float64(idp.clock.Now().Add(defaultLeeway + time.Second).Unix())
		}},
		{"issued in the future", func(idp *fakeIDP, claims map[string]any) {
			claims["iat"] = float64(idp.clock.Now().Add(defaultLeeway + time.Second).Unix())
		}},
		{"missing nonce", func(_ *fakeIDP, claims map[string]any) {
			delete(claims, "nonce")
		}},
		{"nonce mismatch", func(_ *fakeIDP, claims map[string]any) {
			claims["nonce"] = "not-the-transaction-nonce"
		}},
		{"missing subject", func(_ *fakeIDP, claims map[string]any) {
			delete(claims, "sub")
		}},
		{"empty subject", func(_ *fakeIDP, claims map[string]any) {
			claims["sub"] = ""
		}},
		{"at_hash mismatch", func(_ *fakeIDP, claims map[string]any) {
			claims["at_hash"] = "AAAAAAAAAAAAAAAAAAAAAA"
		}},
		{"malformed email_verified", func(_ *fakeIDP, claims map[string]any) {
			claims["email_verified"] = "true"
		}},
		{"malformed nbf", func(_ *fakeIDP, claims map[string]any) {
			claims["nbf"] = "soon"
		}},
		{"malformed auth_time", func(_ *fakeIDP, claims map[string]any) {
			claims["auth_time"] = "earlier"
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			provider := idp.newProvider(t)
			idp.setClaimsBuilder(func(f *fakeIDP) map[string]any {
				claims := f.standardClaims()
				testCase.mutate(f, claims)
				return claims
			})
			_, err := idp.exchange(t, provider)
			requireErrorIs(t, err, ErrInvalidIDToken, testCase.name)
		})
	}
}

func TestExpirationInsideTheLeewayIsAccepted(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setClaimsBuilder(func(f *fakeIDP) map[string]any {
		claims := f.standardClaims()
		claims["exp"] = float64(f.clock.Now().Add(-defaultLeeway + time.Second).Unix())
		return claims
	})
	_, err := idp.exchange(t, provider)
	requireNoError(t, err, "expiration inside the leeway")
}

func TestAtHashValidation(t *testing.T) {
	validHash := func(accessToken string) string {
		sum := sha256.Sum256([]byte(accessToken))
		return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
	}

	t.Run("matching at_hash is accepted", func(t *testing.T) {
		idp := newFakeIDP(t)
		provider := idp.newProvider(t)
		idp.setClaimsBuilder(func(f *fakeIDP) map[string]any {
			claims := f.standardClaims()
			claims["at_hash"] = validHash("access-token")
			return claims
		})
		_, err := idp.exchange(t, provider)
		requireNoError(t, err, "matching at_hash")
	})

	t.Run("mismatched at_hash is rejected", func(t *testing.T) {
		idp := newFakeIDP(t)
		provider := idp.newProvider(t)
		idp.setClaimsBuilder(func(f *fakeIDP) map[string]any {
			claims := f.standardClaims()
			claims["at_hash"] = validHash("another-access-token")
			return claims
		})
		_, err := idp.exchange(t, provider)
		requireErrorIs(t, err, ErrInvalidIDToken, "mismatched at_hash")
	})
}

func TestAccessTokenHashUsesTheAlgorithmHash(t *testing.T) {
	for algorithm, wantLength := range map[string]int{
		"RS256": 16,
		"ES256": 16,
		"PS384": 24,
		"ES512": 32,
	} {
		value, err := accessTokenHash(algorithm, "access-token")
		requireNoError(t, err, algorithm)
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		requireNoError(t, err, algorithm+" decode")
		if len(decoded) != wantLength {
			t.Fatalf("%s at_hash length = %d, want %d", algorithm, len(decoded), wantLength)
		}
	}
}

func TestRejectsUnsignedToken(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setIDTokenOverride(unsignedToken(t, map[string]any{
		"iss": idp.issuer,
		"aud": idp.clientID,
		"sub": "idp-subject",
	}))
	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidIDToken, "alg=none token")
}

func TestRejectsHMACAlgorithmConfusion(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	// The classic confusion attack: sign HS256 with the public key bytes. A
	// verifier that picks the algorithm from the token and treats the JWKS
	// key as an HMAC secret would accept this.
	token := hmacToken(t, idp.publicKeyPEM(t, "rsa-1"), map[string]any{
		"iss": idp.issuer,
		"aud": idp.clientID,
		"sub": "idp-subject",
	})
	idp.setIDTokenOverride(token)
	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidIDToken, "HMAC confusion token")
}
