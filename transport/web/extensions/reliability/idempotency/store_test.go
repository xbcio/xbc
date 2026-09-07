package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
