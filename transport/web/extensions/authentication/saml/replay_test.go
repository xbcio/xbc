package saml

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

func TestReplayCacheClaimsAnAssertionIDOnce(t *testing.T) {
	now := time.Now().UTC()
	cache, err := NewReplayCache(federationweb.NewMemoryStore(), testProvider, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}

	if err := cache.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := cache.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrReplay) {
		t.Fatalf("second Claim = %v, want ErrReplay", err)
	}
	if err := cache.Claim(t.Context(), "_assertion-2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim for another assertion: %v", err)
	}
}

func TestReplayCacheIsolatesProvidersAndDerivesTheReservation(t *testing.T) {
	store := &recordingReservationStore{}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cache, err := NewReplayCache(store, testProvider, WithClock(clock))
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}

	if err := cache.Claim(t.Context(), "_assertion-1", now.Add(90*time.Second)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	reservations := store.reservations()
	if len(reservations) != 1 {
		t.Fatalf("reservations = %v, want one", reservations)
	}
	got := reservations[0]
	if got.namespace != "saml" {
		t.Fatalf("namespace = %q, want saml", got.namespace)
	}
	if got.key != testProvider+"/_assertion-1" {
		t.Fatalf("key = %q, want %q", got.key, testProvider+"/_assertion-1")
	}
	if got.ttl != 90*time.Second {
		t.Fatalf("ttl = %s, want 90s", got.ttl)
	}

	// Two providers over the same store reserve distinct keys, so an assertion
	// from one IdP can never suppress an assertion from another.
	shared := federationweb.NewMemoryStore()
	first, err := NewReplayCache(shared, "one", WithClock(clock))
	if err != nil {
		t.Fatalf("NewReplayCache(one): %v", err)
	}
	second, err := NewReplayCache(shared, "two", WithClock(clock))
	if err != nil {
		t.Fatalf("NewReplayCache(two): %v", err)
	}
	expires := now.Add(time.Minute)
	if err := first.Claim(t.Context(), "_shared", expires); err != nil {
		t.Fatalf("first provider Claim: %v", err)
	}
	if err := second.Claim(t.Context(), "_shared", expires); err != nil {
		t.Fatalf("second provider Claim = %v, want success because the keys are namespaced by provider", err)
	}
}

func TestReplayCacheFailsClosed(t *testing.T) {
	now := time.Now().UTC()

	var typedNil *federationweb.MemoryStore
	for name, store := range map[string]ReservationStore{
		"nil":       nil,
		"typed nil": typedNil,
	} {
		t.Run("constructor rejects "+name, func(t *testing.T) {
			if cache, err := NewReplayCache(store, testProvider); err == nil || cache != nil {
				t.Fatalf("NewReplayCache = %#v, %v; want nil and an error", cache, err)
			}
		})
	}
	for name, provider := range map[string]string{"empty": "", "whitespace": "bad name", "dot": ".."} {
		t.Run("constructor rejects provider "+name, func(t *testing.T) {
			if cache, err := NewReplayCache(federationweb.NewMemoryStore(), provider); err == nil || cache != nil {
				t.Fatalf("NewReplayCache = %#v, %v; want nil and an error", cache, err)
			}
		})
	}

	failing, err := NewReplayCache(failingReservationStore{}, testProvider, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	if err := failing.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrUnavailable) {
		t.Fatalf("store failure = %v, want ErrUnavailable", err)
	}
	if err := failing.Claim(t.Context(), "_assertion-1", time.Time{}); !errors.Is(err, samlcore.ErrUnavailable) {
		t.Fatalf("zero expiry = %v, want ErrUnavailable", err)
	}

	// An empty ID and an already-expired validity window must be refused
	// before the backend is touched. A recording store makes that ordering
	// observable: a reservation attempt would be recorded even when the store
	// itself never fails.
	recording := &recordingReservationStore{}
	guarded, err := NewReplayCache(recording, testProvider, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewReplayCache(recording): %v", err)
	}
	if err := guarded.Claim(t.Context(), "", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrUnavailable) {
		t.Fatalf("empty id = %v, want ErrUnavailable", err)
	}
	if reservations := recording.reservations(); len(reservations) != 0 {
		t.Fatalf("empty id reached the store: %v", reservations)
	}
	if err := guarded.Claim(t.Context(), "_assertion-1", now.Add(-time.Second)); !errors.Is(err, samlcore.ErrUnavailable) {
		t.Fatalf("expired window = %v, want ErrUnavailable", err)
	}
	if reservations := recording.reservations(); len(reservations) != 0 {
		t.Fatalf("expired window reached the store: %v", reservations)
	}
	// A nil context reaches the cache directly here; saml.Verify rejects one
	// before any claim, but the cache contract still fails closed on its own.
	if err := failing.Claim(nil, "_assertion-1", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrNilContext) {
		t.Fatalf("nil context = %v, want ErrNilContext", err)
	}

	var cache *ReplayCache
	if err := cache.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrUnavailable) {
		t.Fatalf("nil cache = %v, want ErrUnavailable", err)
	}
}

func TestReplayCacheConcurrentClaimsSucceedOnce(t *testing.T) {
	now := time.Now().UTC()
	cache, err := NewReplayCache(federationweb.NewMemoryStore(), testProvider, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}

	const claims = 64
	start := make(chan struct{})
	var wait sync.WaitGroup
	var mu sync.Mutex
	accepted, replayed := 0, 0
	wait.Add(claims)
	for index := 0; index < claims; index++ {
		go func() {
			defer wait.Done()
			<-start
			switch err := cache.Claim(context.Background(), "_assertion-race", now.Add(time.Minute)); {
			case err == nil:
				mu.Lock()
				accepted++
				mu.Unlock()
			case errors.Is(err, samlcore.ErrReplay):
				mu.Lock()
				replayed++
				mu.Unlock()
			default:
				t.Errorf("Claim = %v, want nil or ErrReplay", err)
			}
		}()
	}
	close(start)
	wait.Wait()

	if accepted != 1 || replayed != claims-1 {
		t.Fatalf("accepted = %d, replayed = %d; want exactly one accepted claim", accepted, replayed)
	}
}

// TestReplayCacheIsSharedAcrossReplicas pins the multi-replica contract: two
// caches over two stores connected to the same Redis server see each other's
// reservations, and an expired reservation can be claimed again.
func TestReplayCacheIsSharedAcrossReplicas(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	storeA, err := federationweb.NewRedisStore(client, "test:replay:")
	if err != nil {
		t.Fatalf("NewRedisStore(a): %v", err)
	}
	storeB, err := federationweb.NewRedisStore(client, "test:replay:")
	if err != nil {
		t.Fatalf("NewRedisStore(b): %v", err)
	}
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	cacheA, err := NewReplayCache(storeA, testProvider, WithClock(clock))
	if err != nil {
		t.Fatalf("NewReplayCache(a): %v", err)
	}
	cacheB, err := NewReplayCache(storeB, testProvider, WithClock(clock))
	if err != nil {
		t.Fatalf("NewReplayCache(b): %v", err)
	}

	if err := cacheA.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("replica A Claim: %v", err)
	}
	if err := cacheB.Claim(t.Context(), "_assertion-1", now.Add(time.Minute)); !errors.Is(err, samlcore.ErrReplay) {
		t.Fatalf("replica B Claim = %v, want ErrReplay", err)
	}

	// The reservation lives only for the assertion's validity window.
	server.FastForward(2 * time.Minute)
	if err := cacheB.Claim(t.Context(), "_assertion-1", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("Claim after the reservation expired = %v, want success", err)
	}
}
