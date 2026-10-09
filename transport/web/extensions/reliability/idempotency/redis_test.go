package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// TestNewRedisStoreRejectsUnusableConstruction pins the construction guards.
// The typed nil only became reachable once the constructor took the
// topology-neutral interface: a plain nil comparison would let a nil
// *goredis.Client through to fail on the first command instead.
func TestNewRedisStoreRejectsUnusableConstruction(t *testing.T) {
	if store, err := NewRedisStore(nil, "prefix:"); err == nil {
		t.Fatalf("NewRedisStore(nil) = %v, want an error", store)
	}
	var typedNil *goredis.Client
	if store, err := NewRedisStore(typedNil, "prefix:"); err == nil {
		t.Fatalf("NewRedisStore(typed nil) = %v, want an error", store)
	}
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	if store, err := NewRedisStore(client, "bad\nprefix"); err == nil {
		t.Fatalf("NewRedisStore(control prefix) = %v, want an error", store)
	}
	if _, err := NewRedisStore(client, "prefix:"); err != nil {
		t.Fatalf("NewRedisStore() error = %v", err)
	}
}

func TestRedisStoreAtomicLifecycleOwnershipAndTTLTakeover(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisStore(client, "test:idempotency:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := store.Acquire(ctx, "key", "fp", "owner-1", time.Second)
	if err != nil || first.State != Acquired {
		t.Fatalf("first = %#v, %v", first, err)
	}
	pending, _ := store.Acquire(ctx, "key", "fp", "owner-2", time.Second)
	if pending.State != Pending {
		t.Fatalf("pending = %#v", pending)
	}
	if err := store.Release(ctx, "key", "fp", "owner-2"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("wrong owner release = %v", err)
	}
	server.FastForward(2 * time.Second)
	takeover, err := store.Acquire(ctx, "key", "fp", "owner-2", time.Second)
	if err != nil || takeover.State != Acquired {
		t.Fatalf("takeover = %#v, %v", takeover, err)
	}
	response := Response{Status: 202, ContentType: "application/json", Body: []byte(`{"accepted":true}`)}
	if err := store.Complete(ctx, "key", "fp", "owner-1", response, time.Hour); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale owner complete = %v", err)
	}
	if err := store.Complete(ctx, "key", "fp", "owner-2", response, time.Hour); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Acquire(ctx, "key", "fp", "owner-3", time.Second)
	if err != nil || replay.State != Completed || replay.Response.Status != 202 || string(replay.Response.Body) != string(response.Body) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	conflict, _ := store.Acquire(ctx, "key", "other-fp", "owner-4", time.Second)
	if conflict.State != Conflict {
		t.Fatalf("conflict = %#v", conflict)
	}
}
