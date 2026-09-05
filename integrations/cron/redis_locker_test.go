package cron

import (
	"context"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

func newRedisLockerTest(t *testing.T) (*miniredis.Miniredis, *redis.Client, *RedisLocker) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	locker, err := NewRedisLocker(client)
	if err != nil {
		t.Fatalf("NewRedisLocker() error = %v", err)
	}
	return server, client, locker
}

func TestRedisLockerContentionAndOwnerSafeRelease(t *testing.T) {
	server, client, locker := newRedisLockerTest(t)
	ctx := context.Background()
	first, acquired, err := locker.TryAcquire(ctx, "locks:job", time.Second)
	if err != nil || !acquired {
		t.Fatalf("first TryAcquire() = (%v, %v), error = %v", first, acquired, err)
	}
	if _, acquired, err := locker.TryAcquire(ctx, "locks:job", time.Second); err != nil || acquired {
		t.Fatalf("contending TryAcquire() acquired = %v, error = %v", acquired, err)
	}

	// Simulate expiry and takeover. The stale lease must neither renew nor
	// delete the successor's token.
	server.FastForward(2 * time.Second)
	second, acquired, err := locker.TryAcquire(ctx, "locks:job", time.Second)
	if err != nil || !acquired {
		t.Fatalf("takeover TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	if first.Owner() == second.Owner() {
		t.Fatal("two acquisitions reused an owner token")
	}
	if owned, err := first.Renew(ctx, 3*time.Second); err != nil || owned {
		t.Fatalf("stale Renew() owned = %v, error = %v", owned, err)
	}
	if released, err := first.Release(ctx); err != nil || released {
		t.Fatalf("stale Release() released = %v, error = %v", released, err)
	}
	if got, err := client.Get(ctx, "locks:job").Result(); err != nil || got != second.Owner() {
		t.Fatalf("successor value = %q, error = %v; stale owner modified it", got, err)
	}
	if released, err := second.Release(ctx); err != nil || !released {
		t.Fatalf("owner Release() released = %v, error = %v", released, err)
	}
	if server.Exists("locks:job") {
		t.Fatal("lock still exists after owner release")
	}
}

func TestRedisLockerRenewExtendsTTLAndExpiredLockIsTakenOver(t *testing.T) {
	server, _, locker := newRedisLockerTest(t)
	ctx := context.Background()
	lease, acquired, err := locker.TryAcquire(ctx, "locks:long", 100*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	server.FastForward(70 * time.Millisecond)
	if owned, err := lease.Renew(ctx, 200*time.Millisecond); err != nil || !owned {
		t.Fatalf("Renew() owned = %v, error = %v", owned, err)
	}
	if ttl := server.TTL("locks:long"); ttl < 190*time.Millisecond {
		t.Fatalf("TTL after renewal = %s, want approximately 200ms", ttl)
	}

	server.FastForward(250 * time.Millisecond)
	if server.Exists("locks:long") {
		t.Fatal("lease did not expire after renewed TTL")
	}
	successor, acquired, err := locker.TryAcquire(ctx, "locks:long", time.Second)
	if err != nil || !acquired || successor == nil {
		t.Fatalf("post-expiry takeover acquired = %v, lease = %v, error = %v", acquired, successor, err)
	}
}
