package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The memory sweeper reports each session it removes as an EventExpired that
// names the pruned digest and subject, counts one, and carries no ID, attribute
// value, or credential. A still-live session is left alone and reported not at
// all.
func TestMemoryStoreCleanupEmitsExpiredEventsWithoutSensitiveData(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	var events []Event
	store.setObservers([]Observer{observerFunc(func(_ context.Context, event Event) {
		events = append(events, event)
	})})
	ctx := context.Background()

	const secret = "expiry-attribute-value-that-must-never-leak"
	shortID := testID(110, 32)
	short := Session{
		ID:         shortID,
		Subject:    "alice",
		Attributes: map[string]any{"token": secret, "nested": map[string]any{"team": "core"}},
		IssuedAt:   now,
		ExpiresAt:  now.Add(time.Minute),
	}
	if err := store.Create(ctx, short, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	longID := testID(111, 32)
	long := Session{
		ID:        longID,
		Subject:   "bob",
		IssuedAt:  now,
		ExpiresAt: now.Add(2 * time.Hour),
	}
	if err := store.Create(ctx, long, 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	store.cleanup(now)

	if len(events) != 1 {
		t.Fatalf("events = %v, want exactly one expiry", eventKinds(events))
	}
	event := events[0]
	if event.Kind != EventExpired || event.Subject != "alice" || event.Digest != IDDigest(shortID) ||
		event.Count != 1 || event.Verdict != VerdictValid || event.Err != nil {
		t.Fatalf("expiry event = %#v, want alice's swept digest with Count 1", event)
	}
	rendered := fmt.Sprintf("%v|%+v|%#v", event, event, event)
	for _, forbidden := range []string{shortID, longID, secret, "core", "token"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("expiry event leaked %q: %s", forbidden, rendered)
		}
	}
	if _, found, err := store.Get(ctx, longID); err != nil || !found {
		t.Fatalf("the live session was swept: found=%v err=%v", found, err)
	}

	events = nil
	store.cleanup(now)
	if len(events) != 0 {
		t.Fatalf("a second sweep reported %v; nothing was left to expire", eventKinds(events))
	}
}

// The plugin installs its observers on the MemoryStore it owns, so the periodic
// sweep reaches the same observers as the operation events.
func TestPluginWiresExpiryEventsToItsMemoryStore(t *testing.T) {
	var events []Event
	p, err := New(DefaultConfig(), WithObserver(observerFunc(func(_ context.Context, event Event) {
		events = append(events, event)
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	created, err := p.Manager().Create(context.Background(), "alice", map[string]any{"token": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	p.owned.cleanup(time.Now().Add(48 * time.Hour))

	var expired []Event
	for _, event := range events {
		if event.Kind == EventExpired {
			expired = append(expired, event)
		}
	}
	if len(expired) != 1 || expired[0].Digest != IDDigest(created.ID) {
		t.Fatalf("events = %#v, want one expiry for the created session", events)
	}
}

// A MemoryStore injected with WithStore is wired the same way, even though its
// lifecycle remains caller-owned.
func TestPluginWiresExpiryEventsToAnInjectedMemoryStore(t *testing.T) {
	store, err := NewMemoryStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var events []Event
	p, err := New(DefaultConfig(),
		WithStore(store),
		WithObserver(observerFunc(func(_ context.Context, event Event) {
			events = append(events, event)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	created, err := p.Manager().Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	store.cleanup(time.Now().Add(48 * time.Hour))
	var expired []Event
	for _, event := range events {
		if event.Kind == EventExpired {
			expired = append(expired, event)
		}
	}
	if len(expired) != 1 || expired[0].Digest != IDDigest(created.ID) {
		t.Fatalf("events = %#v, want one expiry for the created session", events)
	}
}

// A stopped store reports nothing, so a shutdown never fires late expiry events.
func TestMemoryStoreCleanupAfterCloseEmitsNothing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	var events []Event
	store.setObservers([]Observer{observerFunc(func(_ context.Context, event Event) {
		events = append(events, event)
	})})
	ctx := context.Background()
	if err := store.Create(ctx, sessionValue(testID(112, 32), "alice", now, time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store.cleanup(now.Add(time.Hour))
	if len(events) != 0 {
		t.Fatalf("a closed store emitted %v", eventKinds(events))
	}
}
