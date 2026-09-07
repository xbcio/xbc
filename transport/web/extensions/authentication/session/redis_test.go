package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func newRedisStoreTest(t *testing.T, now *time.Time) (*miniredis.Miniredis, *goredis.Client, *RedisStore) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisStore(client, "test:session:")
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return *now }
	return server, client, store
}

func TestRedisStoreUsesDigestKeysAndDefensiveSerializedValues(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	id := testID(30, 32)
	value := sessionValue(id, "alice", now, time.Hour)
	if err := store.Create(context.Background(), value, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	keys := server.Keys()
	if len(keys) != 1 || keys[0] != "test:session:"+IDDigest(id) || strings.Contains(keys[0], id) {
		t.Fatalf("Redis keys = %#v", keys)
	}
	payload := server.HGet(keys[0], "value")
	if strings.Contains(payload, id) {
		t.Fatal("stored payload leaked the bearer session ID")
	}
	value.Attributes["nested"].(map[string]any)["team"] = "mutated"
	got, found, err := store.Get(context.Background(), id)
	if err != nil || !found || got.Attributes["nested"].(map[string]any)["team"] != "core" {
		t.Fatalf("Get()=%#v found=%v err=%v", got, found, err)
	}
	got.Attributes["nested"].(map[string]any)["team"] = "reader-mutated"
	again, _, _ := store.Get(context.Background(), id)
	if again.Attributes["nested"].(map[string]any)["team"] != "core" {
		t.Fatal("Redis result attributes were aliased")
	}
	if err := store.Create(context.Background(), sessionValue(id, "other", now, time.Hour), time.Minute); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v", err)
	}
}

func TestRedisStoreTouchThrottlesAndCapsIdleTTLAtAbsoluteExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	id := testID(31, 32)
	if err := store.Create(context.Background(), sessionValue(id, "alice", now, time.Hour), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	key := store.key(id)
	if ttl := server.TTL(key); ttl != 10*time.Minute {
		t.Fatalf("initial TTL=%v", ttl)
	}
	server.FastForward(2 * time.Minute)
	now = now.Add(2 * time.Minute)
	if _, found, err := store.Touch(context.Background(), id, 10*time.Minute, 5*time.Minute); err != nil || !found {
		t.Fatalf("throttled Touch found=%v err=%v", found, err)
	}
	if ttl := server.TTL(key); ttl != 8*time.Minute {
		t.Fatalf("throttled TTL=%v, want 8m", ttl)
	}
	server.FastForward(4 * time.Minute)
	now = now.Add(4 * time.Minute)
	if _, found, err := store.Touch(context.Background(), id, 10*time.Minute, 5*time.Minute); err != nil || !found {
		t.Fatalf("renewing Touch found=%v err=%v", found, err)
	}
	if ttl := server.TTL(key); ttl != 10*time.Minute {
		t.Fatalf("renewed TTL=%v, want 10m", ttl)
	}

	nearID := testID(32, 32)
	if err := store.Create(context.Background(), sessionValue(nearID, "bob", now, 3*time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := server.TTL(store.key(nearID)); ttl != 3*time.Minute {
		t.Fatalf("absolute-capped TTL=%v, want 3m", ttl)
	}
	server.FastForward(4 * time.Minute)
	now = now.Add(4 * time.Minute)
	if _, found, err := store.Touch(context.Background(), nearID, 10*time.Minute, 0); err != nil || found {
		t.Fatalf("expired Touch found=%v err=%v", found, err)
	}
}

func TestRedisStoreRotateIsAtomicAcrossConcurrentCallers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, _, store := newRedisStoreTest(t, &now)
	oldID := testID(33, 32)
	if err := store.Create(context.Background(), sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	newIDs := make([]string, 24)
	for i := range newIDs {
		newIDs[i] = testID(byte(50+i), 32)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			err := store.Rotate(context.Background(), oldID, sessionValue(id, "alice", now, time.Hour), 30*time.Minute)
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("Rotate error = %v", err)
			}
		}(newIDs[i])
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful rotations=%d, want 1", successes.Load())
	}
	if _, found, _ := store.Get(context.Background(), oldID); found {
		t.Fatal("old ID survived Redis rotation")
	}
	var replacements int
	for _, id := range newIDs {
		if _, found, err := store.Get(context.Background(), id); err != nil {
			t.Fatal(err)
		} else if found {
			replacements++
		}
	}
	if replacements != 1 {
		t.Fatalf("replacement records=%d, want 1", replacements)
	}
}

func TestRedisStoreValidatesConstructionAndContext(t *testing.T) {
	if _, err := NewRedisStore(nil, "prefix:"); err == nil {
		t.Fatal("nil Redis client accepted")
	}
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	if _, err := NewRedisStore(client, "bad\nprefix"); err == nil {
		t.Fatal("control character in prefix accepted")
	}
	store, err := NewRedisStore(client, "prefix:")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Get(ctx, testID(1, 32)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get canceled error=%v", err)
	}
}
