package saml

import (
	"context"
	"errors"
	"fmt"
	"time"

	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
)

// replayNamespace is the reservation namespace every assertion ID lives under.
// Namespaces and keys occupy disjoint segments of the shared store, and this
// constant keeps an assertion ID from ever colliding with a login transaction.
const replayNamespace = "saml"

// ReservationStore is the slice of the shared federation login base a replay
// cache needs: an atomic set-if-absent reservation with a TTL. The
// federationweb.Login the adapter consumes satisfies this interface, and tests
// or an embedding application may supply any equivalent store.
type ReservationStore interface {
	// SetIfAbsent writes key under namespace only when it is absent and reports
	// whether this call created it.
	SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error)
}

// ReplayCache is a saml.ReplayCache backed by the shared federation login
// base's atomic reservation store. An application injects one into each
// provider's saml.Config.ReplayCache when it constructs that provider:
//
//	cache, err := samlweb.NewReplayCache(login, "corp")
//	if err != nil {
//		return err
//	}
//	provider, err := saml.New(saml.Config{
//		Name:        "corp",
//		// ...
//		ReplayCache: cache,
//	})
//
// # Multi-replica semantics
//
// An assertion ID is reserved through the backend the shared base was
// configured with. When that backend is Redis (the default), every adapter
// replica that shares the Redis client instance sees the same reservation, so
// one replica rejects an assertion another replica already accepted. When the
// shared base uses the memory backend, the cache protects only the process that
// served the first response; a multi-replica deployment must use Redis.
//
// Assertion IDs are namespaced by provider, so a response for one IdP can never
// suppress an identically named assertion from another. The cache is immutable
// after construction, starts no goroutine, and is safe for concurrent use
// because the reservation itself is atomic in the backend.
type ReplayCache struct {
	store    ReservationStore
	provider string
	now      func() time.Time
}

var _ samlcore.ReplayCache = (*ReplayCache)(nil)

// ReplayCacheOption customizes a directly constructed ReplayCache.
type ReplayCacheOption func(*ReplayCache)

// WithClock injects the clock used to derive a reservation's TTL from the
// assertion's expiry. It should be the same clock the provider's
// saml.Config.Now uses; the zero default is time.Now.
func WithClock(now func() time.Time) ReplayCacheOption {
	return func(cache *ReplayCache) { cache.now = now }
}

// NewReplayCache returns a replay cache for the named provider over store.
// store is normally the federationweb.Login exported by plugins.federation-http.
//
// A nil or typed-nil store, and a provider name outside the registry rule, are
// construction errors: a replay cache that cannot claim an assertion must never
// be mistaken for one that can.
func NewReplayCache(store ReservationStore, provider string, opts ...ReplayCacheOption) (*ReplayCache, error) {
	if nilInterface(store) {
		return nil, errors.New("saml-http: replay cache requires a reservation store")
	}
	if !validProviderName(provider) {
		return nil, fmt.Errorf("saml-http: replay cache provider %q is not a valid provider name", provider)
	}
	cache := &ReplayCache{store: store, provider: provider, now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(cache)
		}
	}
	if cache.now == nil {
		cache.now = time.Now
	}
	return cache, nil
}

// Claim implements saml.ReplayCache. It reports ErrReplay when the assertion ID
// has already been reserved for this provider and fails closed with
// ErrUnavailable for every other condition that prevents a claim, including a
// validity window that has already passed and an expiry beyond the store's
// reservation bound.
func (c *ReplayCache) Claim(ctx context.Context, id string, expires time.Time) error {
	if c == nil || nilInterface(c.store) {
		return fmt.Errorf("%w: replay cache is not configured", samlcore.ErrUnavailable)
	}
	if ctx == nil {
		return samlcore.ErrNilContext
	}
	if id == "" || expires.IsZero() {
		return fmt.Errorf("%w: replay claim needs an ID and an expiry", samlcore.ErrUnavailable)
	}
	ttl := expires.Sub(c.now().UTC())
	if ttl < time.Millisecond {
		return fmt.Errorf("%w: assertion validity window has already passed", samlcore.ErrUnavailable)
	}
	created, err := c.store.SetIfAbsent(ctx, replayNamespace, c.provider+"/"+id, ttl)
	if err != nil {
		// The backend error is safe to wrap: it describes the store, never the
		// assertion. Verification turns any non-replay failure into
		// ErrUnavailable and the login fails closed.
		return fmt.Errorf("%w: replay claim failed: %w", samlcore.ErrUnavailable, err)
	}
	if !created {
		return samlcore.ErrReplay
	}
	return nil
}
