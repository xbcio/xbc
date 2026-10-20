package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestMemoryStoreBoundsEntryCount locks the growth bound: a stream of unique
// keys must not grow the record map without limit. The literal cap mirrors
// defaultMemoryMaxEntries. Every record here is a live pending reservation, so
// the store must refuse newcomers rather than evict one of them.
func TestMemoryStoreBoundsEntryCount(t *testing.T) {
	const cap = 10000
	store := NewMemoryStore()
	ctx := context.Background()
	refused := false
	for i := 0; i < cap+32; i++ {
		if _, err := store.Acquire(ctx, "key-"+strconv.Itoa(i), "fp", "owner", time.Hour); err != nil {
			if !errors.Is(err, errMemoryStoreFull) {
				t.Fatalf("acquire %d: %v", i, err)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatalf("store admitted %d unique keys without refusing any", cap+32)
	}
	store.mu.Lock()
	got := len(store.records)
	store.mu.Unlock()
	if got > cap {
		t.Fatalf("records = %d, want <= %d", got, cap)
	}
}

func TestMemoryStoreConcurrentAcquireAndReplay(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	first, err := store.Acquire(ctx, "key", "fp", "owner-1", time.Minute)
	if err != nil || first.State != Acquired {
		t.Fatalf("first = %#v, %v", first, err)
	}
	second, err := store.Acquire(ctx, "key", "fp", "owner-2", time.Minute)
	if err != nil || second.State != Pending {
		t.Fatalf("second = %#v, %v", second, err)
	}
	if err := store.Complete(ctx, "key", "fp", "owner-2", Response{Status: 200}, time.Hour); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("wrong owner complete = %v", err)
	}
	want := Response{Status: 201, ContentType: "application/json", Body: []byte(`{"ok":true}`)}
	if err := store.Complete(ctx, "key", "fp", "owner-1", want, time.Hour); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Acquire(ctx, "key", "fp", "owner-3", time.Minute)
	if err != nil || replay.State != Completed || string(replay.Response.Body) != string(want.Body) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	replay.Response.Body[0] = 'x'
	again, _ := store.Acquire(ctx, "key", "fp", "owner-4", time.Minute)
	if string(again.Response.Body) != string(want.Body) {
		t.Fatal("stored response body was not defensively copied")
	}
	conflict, _ := store.Acquire(ctx, "key", "different", "owner-5", time.Minute)
	if conflict.State != Conflict {
		t.Fatalf("conflict state = %v", conflict.State)
	}
}

func TestMemoryStoreClonesReplayHeaders(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if _, err := store.Acquire(ctx, "key", "fp", "owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Location", "/orders/1")
	header.Set("Cache-Control", "no-store")
	if err := store.Complete(ctx, "key", "fp", "owner", Response{Status: 201, ContentType: "application/json", Header: header}, time.Hour); err != nil {
		t.Fatal(err)
	}
	header.Set("Location", "/mutated")
	first, err := store.Acquire(ctx, "key", "fp", "other", time.Minute)
	if err != nil || first.State != Completed {
		t.Fatalf("first = %#v, %v", first, err)
	}
	if got := first.Response.Header.Get("Location"); got != "/orders/1" {
		t.Fatalf("stored Location = %q, want the value captured at completion", got)
	}
	first.Response.Header.Set("Location", "/mutated-again")
	second, _ := store.Acquire(ctx, "key", "fp", "third", time.Minute)
	if got := second.Response.Header.Get("Location"); got != "/orders/1" {
		t.Fatalf("replayed Location = %q, want a defensive copy", got)
	}
}

func TestMemoryStoreCapacityPrefersCompletedRecords(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewMemoryStore()
	store.maxEntries = 2
	store.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := store.Acquire(ctx, "completed", "fp", "owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, "completed", "fp", "owner", Response{Status: 200}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx, "pending", "fp", "owner", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := store.Acquire(ctx, "fresh", "fp", "owner", time.Minute)
	if err != nil || result.State != Acquired {
		t.Fatalf("fresh = %#v, %v", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.records["completed"]; ok {
		t.Fatal("completed record was not evicted first")
	}
	if _, ok := store.records["pending"]; !ok {
		t.Fatal("pending reservation was evicted before the completed record")
	}
	if _, ok := store.records["fresh"]; !ok {
		t.Fatal("new reservation was not admitted")
	}
}

func TestMemoryStoreFullOfPendingReservationsRefusesNewKeys(t *testing.T) {
	store := NewMemoryStore()
	store.maxEntries = 2
	ctx := context.Background()
	for _, key := range []string{"first", "second"} {
		if _, err := store.Acquire(ctx, key, "fp", "owner", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Acquire(ctx, "third", "fp", "owner", time.Minute); !errors.Is(err, errMemoryStoreFull) {
		t.Fatalf("acquire on a full store = %v, want errMemoryStoreFull", err)
	}
	if result, err := store.Acquire(ctx, "first", "fp", "other", time.Minute); err != nil || result.State != Pending {
		t.Fatalf("existing key = %#v, %v, want Pending", result, err)
	}
}

func TestMemoryStoreCapacityReclaimsExpiredRecords(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewMemoryStore()
	store.maxEntries = 4
	store.now = func() time.Time { return now }
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c", "d"} {
		if _, err := store.Acquire(ctx, key, "fp", "owner", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Second)
	result, err := store.Acquire(ctx, "fresh", "fp", "owner", time.Minute)
	if err != nil || result.State != Acquired {
		t.Fatalf("fresh = %#v, %v", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.records); got != 1 {
		t.Fatalf("records = %d, want only the fresh reservation", got)
	}
}

func TestMemoryStoreBoundedUnderConcurrentAcquire(t *testing.T) {
	store := NewMemoryStore()
	store.maxEntries = 16
	ctx := context.Background()
	var group sync.WaitGroup
	for worker := range 8 {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for i := range 100 {
				// Errors are expected once every record is a live pending
				// reservation; this test pins the bound and the race-freedom of
				// admission, not the success count.
				_, _ = store.Acquire(ctx, fmt.Sprintf("key-%d-%d", worker, i), "fp", "owner", time.Minute)
			}
		}(worker)
	}
	group.Wait()
	store.mu.Lock()
	got := len(store.records)
	store.mu.Unlock()
	if got > 16 {
		t.Fatalf("records = %d, want <= 16", got)
	}
}

// TestMemoryStoreEvictsTheCompletedRecordClosestToExpiry pins the victim
// order among completed records: the one that would expire first goes first.
// Evicting the farthest-expiring record instead would leave the dead weight
// behind and is the mutation this test exists to kill.
func TestMemoryStoreEvictsTheCompletedRecordClosestToExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewMemoryStore()
	store.maxEntries = 3
	store.now = func() time.Time { return now }
	ctx := context.Background()
	complete := func(key string, ttl time.Duration) {
		t.Helper()
		if _, err := store.Acquire(ctx, key, "fp", "owner", time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := store.Complete(ctx, key, "fp", "owner", Response{Status: 200}, ttl); err != nil {
			t.Fatal(err)
		}
	}
	complete("early", 10*time.Minute)
	complete("late", 2*time.Hour)
	if _, err := store.Acquire(ctx, "pending", "fp", "owner", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := store.Acquire(ctx, "fresh", "fp", "owner", time.Minute)
	if err != nil || result.State != Acquired {
		t.Fatalf("fresh = %#v, %v", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.records["early"]; ok {
		t.Fatal("the completed record closest to expiry was not evicted")
	}
	for _, key := range []string{"late", "pending", "fresh"} {
		if _, ok := store.records[key]; !ok {
			t.Fatalf("%q was evicted, want it retained", key)
		}
	}
}

// TestZeroValueMemoryStoreUsesDefaultLimit pins that the zero value is bounded
// too: a zero maxEntries means the package default, never "unlimited".
func TestZeroValueMemoryStoreUsesDefaultLimit(t *testing.T) {
	store := &MemoryStore{}
	ctx := context.Background()
	refused := false
	for i := 0; i < defaultMemoryMaxEntries+32; i++ {
		if _, err := store.Acquire(ctx, "zero-"+strconv.Itoa(i), "fp", "owner", time.Hour); err != nil {
			if !errors.Is(err, errMemoryStoreFull) {
				t.Fatalf("acquire %d: %v", i, err)
			}
			if i < defaultMemoryMaxEntries {
				t.Fatalf("zero-value store refused key %d, want it to admit the default limit first", i)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatalf("zero-value store admitted more than %d pending reservations", defaultMemoryMaxEntries)
	}
	store.mu.Lock()
	got := len(store.records)
	store.mu.Unlock()
	if got > defaultMemoryMaxEntries {
		t.Fatalf("records = %d, want <= %d", got, defaultMemoryMaxEntries)
	}
}

func TestMemoryStoreTTLTakeoverIsOwnerSafe(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewMemoryStore()
	store.now = func() time.Time { return now }
	ctx := context.Background()
	if result, _ := store.Acquire(ctx, "key", "fp", "old", time.Second); result.State != Acquired {
		t.Fatalf("initial state = %v", result.State)
	}
	now = now.Add(2 * time.Second)
	if result, _ := store.Acquire(ctx, "key", "fp", "new", time.Minute); result.State != Acquired {
		t.Fatalf("takeover state = %v", result.State)
	}
	if err := store.Release(ctx, "key", "fp", "old"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("old release = %v", err)
	}
	if err := store.Complete(ctx, "key", "fp", "new", Response{Status: 204}, time.Hour); err != nil {
		t.Fatal(err)
	}
}
