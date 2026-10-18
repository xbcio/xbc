package oidc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJWKSCacheHonorsTTL(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)

	_, err := idp.exchange(t, provider)
	requireNoError(t, err, "first Exchange")
	if jwks, _ := idp.counts(); jwks != 1 {
		t.Fatalf("JWKS fetches after the first Exchange = %d, want 1", jwks)
	}

	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "second Exchange")
	if jwks, _ := idp.counts(); jwks != 1 {
		t.Fatalf("a cached key set was fetched again: JWKS fetches = %d, want 1", jwks)
	}

	idp.clock.Advance(defaultJWKSCacheTTL + time.Second)
	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "Exchange after the cache TTL")
	if jwks, _ := idp.counts(); jwks != 2 {
		t.Fatalf("JWKS fetches after the cache expired = %d, want 2", jwks)
	}
}

func TestUnknownKeyIDRefreshIsRateLimited(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	rogue := generateRSAKey(t, "unpublished-key")
	idp.setSignKey(&rogue)

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUnknownKey, "first unknown key ID")
	if jwks, _ := idp.counts(); jwks != 1 {
		t.Fatalf("JWKS fetches after the first unknown key ID = %d, want 1", jwks)
	}

	// A second verification within the refresh interval must be answered
	// from the cached set: an unknown key ID is not allowed to drive one
	// outbound request per token.
	_, err = idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUnknownKey, "rate-limited unknown key ID")
	if jwks, _ := idp.counts(); jwks != 1 {
		t.Fatalf("a rate-limited unknown key ID reached the JWKS endpoint: fetches = %d, want 1", jwks)
	}

	idp.clock.Advance(defaultJWKSMinRefreshInterval + time.Second)
	_, err = idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUnknownKey, "unknown key ID after the refresh interval")
	if jwks, _ := idp.counts(); jwks != 2 {
		t.Fatalf("JWKS fetches after the refresh interval = %d, want 2", jwks)
	}
}

func TestKeyRotationKeepsPublishedKeysUsable(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	oldKey := idp.keys[0]
	newKey := idp.keys[1]

	// Both keys are published at once, which is what a rotation window looks
	// like: tokens signed with either key verify.
	_, err := idp.exchange(t, provider)
	requireNoError(t, err, "token signed with the old key")
	idp.setSignKid(newKey.kid)
	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "token signed with the new key during the rotation window")

	// The IdP withdraws the old key; after the cache expires it is no longer
	// usable, while the new key still verifies.
	idp.setKeys([]signingKey{newKey})
	idp.clock.Advance(defaultJWKSCacheTTL + time.Second)
	_, err = idp.exchange(t, provider)
	requireNoError(t, err, "token signed with the new key after rotation")

	idp.setSignKid(oldKey.kid)
	idp.setSignKey(&oldKey)
	_, err = idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUnknownKey, "token signed with the withdrawn key")
}

func TestExpiredCacheWithFailedRefreshFailsClosed(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)

	_, err := idp.exchange(t, provider)
	requireNoError(t, err, "initial Exchange")

	idp.setJWKSStatus(500)
	idp.clock.Advance(defaultJWKSCacheTTL + time.Second)
	_, err = idp.exchange(t, provider)
	requireErrorIs(t, err, ErrJWKS, "expired cache with a failing JWKS endpoint")
}

func TestConcurrentVerificationsShareOneJWKSFetch(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	transaction := idp.begin(t, provider)

	const goroutines = 32
	errs := make([]error, goroutines)
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for index := 0; index < goroutines; index++ {
		go func() {
			defer wait.Done()
			_, errs[index] = provider.Exchange(context.Background(), "auth-code", transaction)
		}()
	}
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Exchange %d failed: %v", index, err)
		}
	}
	if jwks, tokenHits := idp.counts(); jwks != 1 {
		t.Fatalf("concurrent verifications made %d JWKS fetches, want 1 (token hits: %d)", jwks, tokenHits)
	}
}

func TestParseKeySetSkipsUnsupportedKeys(t *testing.T) {
	idp := newFakeIDP(t)
	public, err := json.Marshal(idp.keys[0].public)
	requireNoError(t, err, "marshal public key")
	body := []byte(`{"keys":[{"kty":"XYZ","kid":"unsupported"},` + string(public) + `]}`)
	set, err := parseKeySet(body)
	requireNoError(t, err, "parse key set")
	if len(set.Keys) != 1 || set.Keys[0].KeyID != "rsa-1" {
		t.Fatalf("parsed keys = %+v, want only rsa-1", set.Keys)
	}
}

func TestParseKeySetRejectsAnEmptySet(t *testing.T) {
	_, err := parseKeySet([]byte(`{"keys":[]}`))
	requireErrorIs(t, err, ErrJWKS, "empty key set")
}

func TestJWKSSelectionSkipsSymmetricAndMisusedKeys(t *testing.T) {
	set := joseKeySetWithSymmetric(t, "HS256")
	if keys := selectSigningKeys(set, "shared-kid", "RS256"); len(keys) != 0 {
		t.Fatalf("a symmetric key was selected for RS256: %+v", keys)
	}
	// A symmetric key that declares no algorithm passes the algorithm filter;
	// only the symmetric-key guard can reject it.
	untyped := joseKeySetWithSymmetric(t, "")
	if keys := selectSigningKeys(untyped, "shared-kid", "RS256"); len(keys) != 0 {
		t.Fatalf("a symmetric key without a declared algorithm was selected for RS256: %+v", keys)
	}
	// A symmetric key that misdeclares the token's algorithm is rejected by the
	// same guard.
	misdeclared := joseKeySetWithSymmetric(t, "RS256")
	if keys := selectSigningKeys(misdeclared, "shared-kid", "RS256"); len(keys) != 0 {
		t.Fatalf("a symmetric key that declared RS256 was selected: %+v", keys)
	}
	key := generateRSAKey(t, "rsa-1")
	set = joseKeySetOf(key.public)
	if keys := selectSigningKeys(set, "rsa-1", "ES256"); len(keys) != 0 {
		t.Fatal("a key that declares RS256 was selected for an ES256 token")
	}
	if keys := selectSigningKeys(set, "rsa-1", "RS256"); len(keys) != 1 {
		t.Fatalf("the RSA key was not selected for RS256: %+v", keys)
	}
}

func TestJWKSResponseIsNotEchoedInErrors(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	const marker = "jwks-response-marker"
	idp.mu.Lock()
	idp.jwksBody = func() []byte { return []byte(`{"keys":[{"kty":"` + marker + `"}]}`) }
	idp.mu.Unlock()

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrJWKS, "unusable JWKS")
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("JWKS error echoed the response body: %v", err)
	}
}
