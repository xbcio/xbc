package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// keyCacheSnapshot is one consistent read of the JWKS cache state. The four
// fields move together under Provider.mu, so a decision never mixes a key set
// with another fetch's timestamps.
type keyCacheSnapshot struct {
	keys        jose.JSONWebKeySet
	uri         string
	fetched     bool
	fetchedAt   time.Time
	attempted   bool
	attemptedAt time.Time
}

// keyCache reads the current cache state.
func (p *Provider) keyCache() keyCacheSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return keyCacheSnapshot{
		keys:        p.keys,
		uri:         p.keysURI,
		fetched:     p.keysFetched,
		fetchedAt:   p.keysFetchedAt,
		attempted:   p.keysAttempted,
		attemptedAt: p.keysAttemptedAt,
	}
}

// fresh reports whether the fetched key set is still inside its TTL and was
// fetched from the key set URL the current metadata names.
func (s keyCacheSnapshot) fresh(now time.Time, ttl time.Duration, uri string) bool {
	return s.fetched && s.uri == uri && now.Sub(s.fetchedAt) < ttl
}

// mayRefresh reports whether enough time has passed since the last fetch
// attempt. The limit applies to failed attempts too, so an unreachable IdP
// cannot be retried once per login.
func (s keyCacheSnapshot) mayRefresh(now time.Time, interval time.Duration) bool {
	return !s.attempted || now.Sub(s.attemptedAt) >= interval
}

// signingKeys returns every usable key matching kid for the ID token's
// signature algorithm. The common case is served from the cached key set. A
// missing key or an expired cache triggers a refresh, serialized so concurrent
// verifications share one outbound request and rate limited so unknown key IDs
// cannot be used as an amplifier.
func (p *Provider) signingKeys(ctx context.Context, kid, algorithm string) ([]jose.JSONWebKey, error) {
	metadata, ok := p.discovered()
	if !ok {
		return nil, fmt.Errorf("%w: call Discover before Exchange", ErrNotDiscovered)
	}

	// Fast path: a fresh cache that already holds the key.
	if keys, ok := p.cachedSigningKeys(p.now(), kid, algorithm, metadata.jwksURI); ok {
		return keys, nil
	}

	p.keysMu.Lock()
	defer p.keysMu.Unlock()

	// Another verification may have refreshed the cache while this one waited.
	now := p.now()
	if keys, ok := p.cachedSigningKeys(now, kid, algorithm, metadata.jwksURI); ok {
		return keys, nil
	}

	snapshot := p.keyCache()
	if !snapshot.mayRefresh(now, p.jwksRefresh) {
		if snapshot.fresh(now, p.jwksTTL, metadata.jwksURI) {
			return nil, fmt.Errorf("%w: the key set was fetched less than %s ago", ErrUnknownKey, p.jwksRefresh)
		}
		return nil, fmt.Errorf("%w: the cached key set is expired or superseded and refresh is limited to one per %s", ErrJWKS, p.jwksRefresh)
	}

	p.mu.Lock()
	p.keysAttempted = true
	p.keysAttemptedAt = now
	p.mu.Unlock()

	keySet, err := p.fetchJWKS(ctx, metadata.jwksURI)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.keys = keySet
	p.keysURI = metadata.jwksURI
	p.keysFetched = true
	p.keysFetchedAt = now
	p.mu.Unlock()

	keys := selectSigningKeys(keySet, kid, algorithm)
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, kid)
	}
	return keys, nil
}

// cachedSigningKeys returns the keys matching kid when the cache is fresh.
func (p *Provider) cachedSigningKeys(now time.Time, kid, algorithm, uri string) ([]jose.JSONWebKey, bool) {
	snapshot := p.keyCache()
	if !snapshot.fresh(now, p.jwksTTL, uri) {
		return nil, false
	}
	keys := selectSigningKeys(snapshot.keys, kid, algorithm)
	return keys, len(keys) > 0
}

// fetchJWKS retrieves and parses the key set at jwksURI through the outbound
// policy client.
func (p *Provider) fetchJWKS(ctx context.Context, jwksURI string) (jose.JSONWebKeySet, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: %v", ErrJWKS, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: %v", ErrJWKS, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: JWKS endpoint returned status %d", ErrJWKS, response.StatusCode)
	}
	body, err := outbound.ReadBody(response, p.policy.MaxBodyBytes)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: %v", ErrJWKS, err)
	}
	return parseKeySet(body)
}

// parseKeySet parses a JWKS leniently per RFC 7517: a key this implementation
// does not understand is skipped instead of failing the whole set, so one
// exotic key cannot disable an otherwise usable key set. A set with no usable
// key is rejected.
func parseKeySet(body []byte) (jose.JSONWebKeySet, error) {
	var envelope struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: key set is not a JSON object", ErrJWKS)
	}
	var set jose.JSONWebKeySet
	for _, raw := range envelope.Keys {
		var key jose.JSONWebKey
		if err := json.Unmarshal(raw, &key); err != nil {
			continue
		}
		if !key.Valid() {
			continue
		}
		set.Keys = append(set.Keys, key)
	}
	if len(set.Keys) == 0 {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: key set carries no usable key", ErrJWKS)
	}
	return set, nil
}

// selectSigningKeys returns the keys that may verify a signature with the ID
// token's algorithm. Keys are selected by key ID; a key that declares a
// different use or algorithm is skipped, and symmetric keys are skipped
// outright so a JWKS can never supply an HMAC secret for an asymmetric
// algorithm.
func selectSigningKeys(set jose.JSONWebKeySet, kid, algorithm string) []jose.JSONWebKey {
	var keys []jose.JSONWebKey
	for _, key := range set.Keys {
		if key.KeyID != kid {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if key.Algorithm != "" && algorithm != "" && key.Algorithm != algorithm {
			continue
		}
		if _, symmetric := key.Key.([]byte); symmetric {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}
