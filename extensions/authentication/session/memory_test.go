package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newControlledMemoryStore(t *testing.T, now *time.Time) *MemoryStore {
	t.Helper()
	store, err := NewMemoryStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return *now }
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMemoryStoreLifecycleTouchThrottleAndDefensiveCopies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	id := testID(1, 32)
	value := sessionValue(id, "alice", now, time.Hour)
	if err := store.Create(context.Background(), value, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	value.Attributes["role"] = "mutated"
	got, found, err := store.Get(context.Background(), id)
	if err != nil || !found || got.Attributes["role"] != "admin" {
		t.Fatalf("Get() = %#v, %v, %v", got, found, err)
	}
	got.Attributes["role"] = "reader"
	got.Attributes["nested"].(map[string]any)["team"] = "other"
	again, _, _ := store.Get(context.Background(), id)
	if again.Attributes["role"] != "admin" || again.Attributes["nested"].(map[string]any)["team"] != "core" {
		t.Fatalf("stored attributes were aliased: %#v", again.Attributes)
	}

	key := IDDigest(id)
	firstExpiry := store.records[key].idleExpiresAt
	now = now.Add(2 * time.Minute)
	if _, found, err := store.Touch(context.Background(), id, 10*time.Minute, 5*time.Minute); err != nil || !found {
		t.Fatalf("early Touch() found=%v err=%v", found, err)
	}
	if !store.records[key].idleExpiresAt.Equal(firstExpiry) {
		t.Fatal("throttled Touch extended idle lease")
	}
	now = now.Add(4 * time.Minute)
	if _, found, err := store.Touch(context.Background(), id, 10*time.Minute, 5*time.Minute); err != nil || !found {
		t.Fatalf("renewing Touch() found=%v err=%v", found, err)
	}
	if want := now.Add(10 * time.Minute); !store.records[key].idleExpiresAt.Equal(want) {
		t.Fatalf("idle expiry = %v, want %v", store.records[key].idleExpiresAt, want)
	}
	now = now.Add(11 * time.Minute)
	if _, found, err := store.Get(context.Background(), id); err != nil || found {
		t.Fatalf("expired Get() found=%v err=%v", found, err)
	}
}

func TestMemoryStoreRotateIsAtomicAndPreventsFixation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	oldID := testID(2, 32)
	if err := store.Create(context.Background(), sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			replacement := sessionValue(testID(byte(index+10), 32), "alice", now, time.Hour)
			err := store.Rotate(context.Background(), oldID, replacement, 30*time.Minute)
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("Rotate() error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful rotations = %d, want 1", successes.Load())
	}
	if _, found, _ := store.Get(context.Background(), oldID); found {
		t.Fatal("old ID survived rotation")
	}
}

func TestMemoryStoreConcurrentCloseIsIdempotentAndErasesState(t *testing.T) {
	store, err := NewMemoryStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Close(); err != nil {
				t.Errorf("Close() = %v", err)
			}
		}()
	}
	wg.Wait()
	if err := store.Create(context.Background(), sessionValue(testID(3, 32), "alice", time.Now(), time.Hour), time.Minute); !errors.Is(err, ErrStopped) {
		t.Fatalf("Create after close = %v", err)
	}
	if len(store.records) != 0 {
		t.Fatalf("records retained after close: %d", len(store.records))
	}
}
