package redissyncmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/log"
)

// testRecord is the caller-owned value type the tests' transforms decode.
type testRecord struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// recordTransform decodes raw JSON with the caller's own tags, which is the
// transform contract this plugin promises.
func recordTransform(_ string, raw json.RawMessage) (any, error) {
	var record testRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	return record, nil
}

func plainConfig() Config {
	return Config{RedisInstance: "default", Namespace: "test", Persist: "none"}
}

func persistentConfig() Config {
	return Config{RedisInstance: "default", Namespace: "test", Persist: "ttl", TTL: time.Hour}
}

// TestMiniredisSupportsPubSub is the probe the rest of this file depends on:
// the cross-instance tests are only meaningful if the in-process Redis
// implementation actually brokers PUBLISH and SUBSCRIBE. It follows the same
// confirmation sequence the map uses, including the ping that releases the
// RESP3 subscription push.
func TestMiniredisSupportsPubSub(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pubsub := client.Subscribe(ctx, "probe")
	t.Cleanup(func() { _ = pubsub.Close() })
	if err := pubsub.Ping(ctx); err != nil {
		t.Fatalf("reach subscription: %v", err)
	}
	events := pubsub.ChannelWithSubscriptions()
	select {
	case event := <-events:
		if subscription, ok := event.(*redis.Subscription); !ok || subscription.Channel != "probe" {
			t.Fatalf("first event = %#v, want the subscription acknowledgement", event)
		}
	case <-ctx.Done():
		t.Fatal("miniredis did not confirm the subscription")
	}

	if err := client.Publish(ctx, "probe", "hello").Err(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case event := <-events:
		message, ok := event.(*redis.Message)
		if !ok {
			t.Fatalf("event = %#v, want a message", event)
		}
		if message.Payload != "hello" {
			t.Fatalf("payload = %q, want hello", message.Payload)
		}
	case <-ctx.Done():
		t.Fatal("miniredis did not deliver the published message; it does not support pub/sub")
	}
}

func TestSetPropagatesToAnotherInstance(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	second := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	second.start(t)

	if err := firstMap.Set(context.Background(), "one", testRecord{Name: "alpha", Count: 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, secondMap, "one", "alpha")

	local, ok := firstMap.Load("one")
	if !ok {
		t.Fatal("the writing map lost its own entry")
	}
	if local.(testRecord).Count != 1 {
		t.Fatalf("local value = %#v, want the written record", local)
	}
}

func TestDeletePropagatesToAnotherInstance(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	second := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	second.start(t)

	if err := firstMap.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, secondMap, "one", "alpha")
	if err := firstMap.Delete(context.Background(), "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := secondMap.Load("one")
		return !ok
	}, "delete did not reach the second instance")
}

func TestClearPropagatesToAnotherInstance(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	second := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	second.start(t)

	for _, key := range []string{"one", "two"} {
		if err := firstMap.Set(context.Background(), key, testRecord{Name: key}); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}
	eventuallyLoaded(t, secondMap, "one", "one")
	eventuallyLoaded(t, secondMap, "two", "two")
	if err := firstMap.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, hasOne := secondMap.Load("one")
		_, hasTwo := secondMap.Load("two")
		return !hasOne && !hasTwo
	}, "clear did not reach the second instance")
}

// TestSelfPublishedMessagesAreNotTransformedAgain is the self-loop guard. The
// writing map must drop its own notification before the transform runs;
// otherwise every write is applied twice and a transform with side effects
// observes two calls.
func TestSelfPublishedMessagesAreNotTransformedAgain(t *testing.T) {
	server := miniredis.RunT(t)
	var calls atomic.Int64
	countingTransform := func(key string, raw json.RawMessage) (any, error) {
		calls.Add(1)
		return recordTransform(key, raw)
	}

	first := newTestInstance(t, server, plainConfig())
	second := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(countingTransform))
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	second.start(t)

	if err := firstMap.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, secondMap, "one", "alpha")

	// The peer receiving the message proves the publication reached Redis. A
	// short grace period after that lets a self-delivery, if the guard were
	// missing, arrive and increment the counter.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("transform calls = %d, want 1: the map transformed its own notification", got)
	}
}

func TestPersistentMapLoadsSnapshotOnStart(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	if err := firstMap.Set(context.Background(), "persisted", testRecord{Name: "before-start"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	second := newTestInstance(t, server, persistentConfig())
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	second.start(t)

	value, ok := secondMap.Load("persisted")
	if !ok {
		t.Fatal("a map started after the write did not load the persisted snapshot")
	}
	if value.(testRecord).Name != "before-start" {
		t.Fatalf("snapshot value = %#v", value)
	}
}

// TestSubscriptionIsConfirmedBeforeSnapshot injects a change after the
// snapshot read and before the listener starts. The subscription must already
// be established at that point, so the change is buffered and delivered;
// reading the snapshot before subscribing would publish into an unobserved
// window and lose it.
func TestSubscriptionIsConfirmedBeforeSnapshot(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)

	second := newTestInstance(t, server, persistentConfig())
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))

	injected := false
	secondMap.(*syncMap).afterSnapshot = func() {
		injected = true
		if err := firstMap.Set(context.Background(), "window", testRecord{Name: "during-startup"}); err != nil {
			t.Errorf("Set during startup window: %v", err)
		}
	}
	second.start(t)
	if !injected {
		t.Fatal("the startup hook did not run; the window assertion proved nothing")
	}
	eventuallyLoaded(t, secondMap, "window", "during-startup")
}

// TestLocalLRUEvictionDoesNotDeleteOrBroadcast pins eviction as a local
// concern: the evicted entry disappears from this map only, stays in Redis, and
// reaches peers.
func TestLocalLRUEvictionDoesNotDeleteOrBroadcast(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, persistentConfig())
	second := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform), syncmap.WithMaxSize(2))
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	second.start(t)

	for index := range 3 {
		key := fmt.Sprintf("key-%d", index)
		if err := firstMap.Set(context.Background(), key, testRecord{Name: key}); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}

	if _, ok := firstMap.Load("key-0"); ok {
		t.Fatal("the least recently used entry was not evicted")
	}
	if held := firstMap.(*syncMap).store.Len(); held != 2 {
		t.Fatalf("local entries = %d, want 2 after eviction", held)
	}
	for index := range 3 {
		key := fmt.Sprintf("key-%d", index)
		eventuallyLoaded(t, secondMap, key, key)
	}

	hashKey := firstMap.(*syncMap).hashKey
	waitFor(t, 2*time.Second, func() bool {
		keys, err := server.HKeys(hashKey)
		return err == nil && len(keys) == 3
	}, "eviction removed an entry from Redis")

	// The peer keeps every key, which proves eviction broadcast no delete.
	time.Sleep(100 * time.Millisecond)
	if _, ok := secondMap.Load("key-0"); !ok {
		t.Fatal("eviction was broadcast as a delete")
	}
}

// TestTransformFailureLeavesNoLocalOrRemoteEntry covers both directions: a
// local write the transform rejects stores nothing anywhere, and a peer's
// value the transform rejects is skipped rather than stored raw.
func TestTransformFailureLeavesNoLocalOrRemoteEntry(t *testing.T) {
	server := miniredis.RunT(t)
	var remoteAttempts atomic.Int64
	rejectingTransform := func(key string, raw json.RawMessage) (any, error) {
		if key == "bad" {
			remoteAttempts.Add(1)
			return nil, errors.New("rejected by test transform")
		}
		return recordTransform(key, raw)
	}

	first := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(rejectingTransform))
	first.start(t)

	err := firstMap.Set(context.Background(), "bad", testRecord{Name: "local"})
	if !errors.Is(err, syncmap.ErrTransform) {
		t.Fatalf("Set error = %v, want ErrTransform", err)
	}
	if _, ok := firstMap.Load("bad"); ok {
		t.Fatal("a transform failure left a local entry")
	}
	if server.Exists(firstMap.(*syncMap).hashKey) {
		t.Fatal("a transform failure persisted an entry")
	}

	second := newTestInstance(t, server, plainConfig())
	secondMap := second.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	second.start(t)
	if err := secondMap.Set(context.Background(), "bad", testRecord{Name: "remote"}); err != nil {
		t.Fatalf("peer Set: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return remoteAttempts.Load() >= 2 },
		"the first map never attempted the peer's value")
	time.Sleep(100 * time.Millisecond)
	if _, ok := firstMap.Load("bad"); ok {
		t.Fatal("a value the transform rejected was stored locally")
	}
}

// TestPersistenceFailureLeavesLocalStateUnchanged pins the chosen write
// semantics: the backing store is written before the local map, so a store
// failure changes nothing locally.
func TestPersistenceFailureLeavesLocalStateUnchanged(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	if err := firstMap.Set(context.Background(), "kept", testRecord{Name: "kept"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := firstMap.Set(ctx, "lost", testRecord{Name: "lost"})
	if !errors.Is(err, syncmap.ErrPersist) {
		t.Fatalf("Set after store failure = %v, want ErrPersist", err)
	}
	if _, ok := firstMap.Load("lost"); ok {
		t.Fatal("a failed persistence still changed the local map")
	}
	if _, ok := firstMap.Load("kept"); !ok {
		t.Fatal("a failed persistence disturbed an existing local entry")
	}
	if err := firstMap.Delete(ctx, "kept"); !errors.Is(err, syncmap.ErrPersist) {
		t.Fatalf("Delete after store failure = %v, want ErrPersist", err)
	}
	if _, ok := firstMap.Load("kept"); !ok {
		t.Fatal("a failed delete persistence still changed the local map")
	}
	if err := firstMap.Clear(ctx); !errors.Is(err, syncmap.ErrPersist) {
		t.Fatalf("Clear after store failure = %v, want ErrPersist", err)
	}
	if _, ok := firstMap.Load("kept"); !ok {
		t.Fatal("a failed clear persistence still changed the local map")
	}
}

// TestBroadcastFailureReportsErrorAfterLocalChange pins the other half of the
// documented contract: once persistence and the local change have succeeded, a
// failed publication is reported but not rolled back.
func TestBroadcastFailureReportsErrorAfterLocalChange(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)

	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := firstMap.Set(ctx, "local", testRecord{Name: "local"})
	if !errors.Is(err, syncmap.ErrBroadcast) {
		t.Fatalf("Set after broadcast failure = %v, want ErrBroadcast", err)
	}
	if got, ok := firstMap.Load("local"); !ok {
		t.Fatal("the documented semantics are local state already updated; it was not")
	} else if got.(testRecord).Name != "local" {
		t.Fatalf("local value = %#v", got)
	}

	if err := firstMap.Delete(ctx, "local"); !errors.Is(err, syncmap.ErrBroadcast) {
		t.Fatalf("Delete after broadcast failure = %v, want ErrBroadcast", err)
	}
	if _, ok := firstMap.Load("local"); ok {
		t.Fatal("the documented semantics are local state already updated; Delete did not remove it")
	}

	if err := firstMap.Set(ctx, "another", testRecord{Name: "another"}); !errors.Is(err, syncmap.ErrBroadcast) {
		t.Fatalf("Set after broadcast failure = %v, want ErrBroadcast", err)
	}
	if err := firstMap.Clear(ctx); !errors.Is(err, syncmap.ErrBroadcast) {
		t.Fatalf("Clear after broadcast failure = %v, want ErrBroadcast", err)
	}
	if _, ok := firstMap.Load("another"); ok {
		t.Fatal("the documented semantics are local state already updated; Clear did not run")
	}
}

func TestResyncRestoresPersistedSnapshot(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, persistentConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)
	if err := firstMap.Set(context.Background(), "kept", testRecord{Name: "kept"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Simulate the state a disconnected replica can be left in: the entry is
	// gone locally while the store still holds it.
	firstMap.(*syncMap).store.Clear()
	if _, ok := firstMap.Load("kept"); ok {
		t.Fatal("test setup failed to clear the local map")
	}
	if err := firstMap.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if value, ok := firstMap.Load("kept"); !ok || value.(testRecord).Name != "kept" {
		t.Fatalf("Resync did not restore the snapshot: %#v, %t", value, ok)
	}
}

func TestResyncIsUnavailableWithoutPersistence(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)

	if err := firstMap.Resync(context.Background()); !errors.Is(err, syncmap.ErrPersistRequired) {
		t.Fatalf("Resync = %v, want ErrPersistRequired", err)
	}
}

func TestDestroyStopsSubscriptionAndIsIdempotent(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules")
	first.start(t)
	concrete := firstMap.(*syncMap)

	if err := firstMap.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	select {
	case <-concrete.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Destroy returned but the subscription loop did not stop")
	}
	if err := firstMap.Destroy(); err != nil {
		t.Fatalf("repeated Destroy: %v", err)
	}
}

func TestStopIsSafeAfterFailedInitAndBeforeStart(t *testing.T) {
	if err := stopFactory(nil, context.Background()); err != nil {
		t.Fatalf("stopFactory(nil) = %v", err)
	}

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	created, err := newFactory(log.Nop(), client, plainConfig())
	if err != nil {
		t.Fatalf("newFactory: %v", err)
	}
	if err := initFactory(created, nil); err == nil {
		t.Fatal("initFactory with a nil context reported success")
	}
	// A map may exist before Start; Stop must release it without a subscription
	// ever having run.
	if _, err := created.Map("rules"); err != nil {
		t.Fatalf("Map before start: %v", err)
	}
	if err := stopFactory(created, context.Background()); err != nil {
		t.Fatalf("Stop after failed Init: %v", err)
	}
	if err := stopFactory(created, context.Background()); err != nil {
		t.Fatalf("repeated Stop: %v", err)
	}
}

func TestFactoryRefusesMapsOutsideItsCreationWindow(t *testing.T) {
	server := miniredis.RunT(t)
	instance := newTestInstance(t, server, plainConfig())

	if _, err := instance.factory.Map(""); !errors.Is(err, syncmap.ErrInvalidName) {
		t.Fatalf("Map with an empty name = %v, want ErrInvalidName", err)
	}
	if _, err := instance.factory.Map("rules"); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if _, err := instance.factory.Map("rules"); !errors.Is(err, syncmap.ErrDuplicate) {
		t.Fatalf("duplicate Map = %v, want ErrDuplicate", err)
	}
	if _, err := instance.factory.Map("bad-ttl", syncmap.WithPersist(syncmap.PersistTTL)); !errors.Is(err, syncmap.ErrInvalidOption) {
		t.Fatalf("ttl without a ttl = %v, want ErrInvalidOption", err)
	}
	var nilOption syncmap.Option
	if _, err := instance.factory.Map("nil-option", nilOption); !errors.Is(err, syncmap.ErrInvalidOption) {
		t.Fatalf("nil option = %v, want ErrInvalidOption", err)
	}
	if _, err := instance.factory.Map("negative", syncmap.WithMaxSize(-1)); !errors.Is(err, syncmap.ErrInvalidOption) {
		t.Fatalf("negative max size = %v, want ErrInvalidOption", err)
	}

	instance.start(t)
	if _, err := instance.factory.Map("late"); !errors.Is(err, syncmap.ErrStarted) {
		t.Fatalf("Map after Start = %v, want ErrStarted", err)
	}
	if err := stopFactory(instance.factory, context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := instance.factory.Map("closed"); !errors.Is(err, syncmap.ErrClosed) {
		t.Fatalf("Map after Stop = %v, want ErrClosed", err)
	}
}

func TestEmptyKeysAreRejected(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, plainConfig())
	firstMap := first.mapFor(t, "rules", syncmap.WithTransform(recordTransform))
	first.start(t)

	if err := firstMap.Set(context.Background(), "", testRecord{}); !errors.Is(err, syncmap.ErrInvalidKey) {
		t.Fatalf("Set with an empty key = %v, want ErrInvalidKey", err)
	}
	if err := firstMap.Delete(context.Background(), ""); !errors.Is(err, syncmap.ErrInvalidKey) {
		t.Fatalf("Delete with an empty key = %v, want ErrInvalidKey", err)
	}
}
