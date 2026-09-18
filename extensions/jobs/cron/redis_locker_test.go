package cron

import (
	"context"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/plugin"
)

func newRedisLockerTest(t *testing.T) (*miniredis.Miniredis, *redis.Client, lease.Locker) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	locker, err := newRedisLocker(client)
	if err != nil {
		t.Fatalf("newRedisLocker() error = %v", err)
	}
	return server, client, locker
}

func TestRedisLockerRejectsNilClient(t *testing.T) {
	if locker, err := newRedisLocker(nil); err == nil {
		t.Fatalf("newRedisLocker(nil) = (%v, nil), want an error", locker)
	}
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
	if ttl := server.TTL("locks:job"); ttl <= 0 || ttl > time.Second {
		t.Fatalf("successor TTL = %s, want at most 1s; the stale renewal extended it", ttl)
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
	held, acquired, err := locker.TryAcquire(ctx, "locks:long", 100*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	server.FastForward(70 * time.Millisecond)
	if owned, err := held.Renew(ctx, 200*time.Millisecond); err != nil || !owned {
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
	if successor.Key() != "locks:long" {
		t.Fatalf("successor Key() = %q, want %q", successor.Key(), "locks:long")
	}
}

// TestRedisLockerRejectsUnusableArguments covers the guards that keep an
// unusable acquisition from reaching Redis at all: an empty key would collide
// with every other empty-key caller, and a sub-millisecond TTL cannot be
// expressed by SET PX.
func TestRedisLockerRejectsUnusableArguments(t *testing.T) {
	_, _, locker := newRedisLockerTest(t)
	ctx := context.Background()
	if _, acquired, err := locker.TryAcquire(ctx, "", time.Second); err == nil || acquired {
		t.Fatalf("TryAcquire() with an empty key = (%v, %v), want an error", acquired, err)
	}
	if _, acquired, err := locker.TryAcquire(ctx, "locks:short", 999*time.Microsecond); err == nil || acquired {
		t.Fatalf("TryAcquire() with a sub-millisecond TTL = (%v, %v), want an error", acquired, err)
	}
	held, acquired, err := locker.TryAcquire(ctx, "locks:short", time.Second)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	if owned, err := held.Renew(ctx, 999*time.Microsecond); err == nil || owned {
		t.Fatalf("Renew() with a sub-millisecond TTL = (%v, %v), want an error", owned, err)
	}
}

// TestRedisLockerBacksTheDistributedRedisInstancePath covers the other
// convenience key. distributed.redis_instance names a client the plugin graph
// already produced, so cron takes its lease over that client without owning it:
// closing it belongs to the Definition that opened it.
func TestRedisLockerBacksTheDistributedRedisInstancePath(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	config := defaultConfig()
	distributedConfig(&config)
	config.Distributed.RedisInstance = "locks"
	prepared, err := prepareConfig(config)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	job := &funcJob{name: "shared", spec: "@hourly", run: func(context.Context) error { return nil }}
	p, err := newConfiguredPlugin(prepared, []plugin.Entry[JobContributor]{contributorEntry("jobs", job)}, nil, client)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}

	if p.ownedRedis != nil {
		t.Fatal("a graph-provided client must not be owned by cron")
	}
	backing, ok := p.locker.(*redisLocker)
	if !ok {
		t.Fatalf("locker type = %T, want *redisLocker", p.locker)
	}
	if backing.client != client {
		t.Fatal("the locker was not taken over the client the instance named")
	}

	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Stop closed a client cron does not own: %v", err)
	}
}

// TestExternalLockerTakesPriorityOverRedisBackends pins the input path that must
// stay ahead of both convenience keys: a lease.Locker supplied by another
// Definition is used as given, with no Redis client created and no wrapping.
func TestExternalLockerTakesPriorityOverRedisBackends(t *testing.T) {
	supplied := newFakeLocker()
	p := newTestPlugin(t, distributedConfig, supplied,
		&funcJob{name: "external", spec: "@hourly", run: func(context.Context) error { return nil }})
	if p.locker != lease.Locker(supplied) {
		t.Fatalf("locker = %#v, want the supplied lease.Locker", p.locker)
	}
	if p.ownedRedis != nil {
		t.Fatal("an externally supplied locker must not make cron open a Redis client")
	}
}

// TestRedisLockerBacksTheDistributedRedisAddrPath proves the convenience path
// documented on Config actually reaches the locker above: a plugin configured
// with distributed.redis.addr and no external lease.Locker takes its lease over
// its own client, so the key the scheduler would lock is created in Redis.
func TestRedisLockerBacksTheDistributedRedisAddrPath(t *testing.T) {
	server := miniredis.RunT(t)
	job := &funcJob{name: "owned", spec: "@hourly", run: func(context.Context) error { return nil }}
	p := newTestPlugin(t, func(config *Config) {
		distributedConfig(config)
		config.Distributed.Redis.Addr = server.Addr()
	}, nil, job)
	t.Cleanup(func() {
		if err := p.stop(context.Background()); err != nil {
			t.Errorf("stop() error = %v", err)
		}
	})

	if p.ownedRedis == nil {
		t.Fatal("distributed.redis.addr did not make the plugin own a client")
	}
	if _, ok := p.locker.(*redisLocker); !ok {
		t.Fatalf("locker type = %T, want *redisLocker", p.locker)
	}

	ctx := context.Background()
	held, acquired, err := p.locker.TryAcquire(ctx, p.jobs[0].lockKey, p.config.Distributed.TTL)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() over the owned client acquired = %v, error = %v", acquired, err)
	}
	if got := server.Keys(); len(got) != 1 || got[0] != p.jobs[0].lockKey {
		t.Fatalf("keys in Redis = %v, want [%s]", got, p.jobs[0].lockKey)
	}
	if released, err := held.Release(ctx); err != nil || !released {
		t.Fatalf("Release() released = %v, error = %v", released, err)
	}
}
