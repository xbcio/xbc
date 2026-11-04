package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// plainOptions broadcasts only.
func plainOptions() MapOptions[testRecord] { return MapOptions[testRecord]{} }

// persistentOptions stores the collection with an expiry.
func persistentOptions() MapOptions[testRecord] {
	return MapOptions[testRecord]{Persist: PersistTTL, TTL: time.Hour}
}

// TestMiniredisSupportsPubSub is the probe the rest of this file depends on:
// the cross-instance tests are only meaningful if the in-process Redis
// implementation actually brokers PUBLISH and SUBSCRIBE. It follows the same
// confirmation sequence the map uses, including the ping that releases the
// RESP3 subscription push.
func TestMiniredisSupportsPubSub(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)

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
	first := newTestMap(t, server, "rules", plainOptions())
	second := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "one", testRecord{Name: "alpha", Count: 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, second, "one", "alpha")

	local, ok := first.Load("one")
	if !ok {
		t.Fatal("the writing map lost its own entry")
	}
	if local.Count != 1 {
		t.Fatalf("local value = %#v, want the written record", local)
	}
}

func TestDeletePropagatesToAnotherInstance(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())
	second := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, second, "one", "alpha")
	if err := first.Delete(context.Background(), "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := second.Load("one")
		return !ok
	}, "delete did not reach the second instance")
}

func TestClearPropagatesToAnotherInstance(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())
	second := newTestMap(t, server, "rules", plainOptions())

	for _, key := range []string{"one", "two"} {
		if err := first.Set(context.Background(), key, testRecord{Name: key}); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}
	eventuallyLoaded(t, second, "one", "one")
	eventuallyLoaded(t, second, "two", "two")
	if err := first.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, hasOne := second.Load("one")
		_, hasTwo := second.Load("two")
		return !hasOne && !hasTwo
	}, "clear did not reach the second instance")
}

// TestSelfPublishedMessagesAreNotTransformedAgain is the self-loop guard. The
// writing map must drop its own notification before Decode runs; otherwise
// every write is applied twice and a decoder with side effects observes two
// calls.
func TestSelfPublishedMessagesAreNotTransformedAgain(t *testing.T) {
	server := miniredis.RunT(t)
	var calls atomic.Int64
	countingDecoder := func(key string, raw json.RawMessage) (testRecord, error) {
		calls.Add(1)
		return recordDecoder(key, raw)
	}

	first := newTestMap(t, server, "rules", MapOptions[testRecord]{Decode: countingDecoder})
	second := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, second, "one", "alpha")

	// The peer receiving the message proves the publication reached Redis. A
	// short grace period after that lets a self-delivery, if the guard were
	// missing, arrive and increment the counter.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("Decode calls = %d, want 1: the map decoded its own notification", got)
	}
}

func TestPersistentMapLoadsSnapshotOnStart(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Set(context.Background(), "persisted", testRecord{Name: "before-start"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	second := newTestMap(t, server, "rules", persistentOptions())

	value, ok := second.Load("persisted")
	if !ok {
		t.Fatal("a map started after the write did not load the persisted snapshot")
	}
	if value.Name != "before-start" {
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
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Set(context.Background(), "before", testRecord{Name: "before-start"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	second, err := newMap(newTestClient(t, server), "rules", persistentOptions())
	if err != nil {
		t.Fatalf("newMap: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	injected := false
	second.afterSnapshot = func() {
		injected = true
		if err := first.Set(context.Background(), "window", testRecord{Name: "during-startup"}); err != nil {
			t.Errorf("Set during startup window: %v", err)
		}
	}
	if err := second.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !injected {
		t.Fatal("the startup hook did not run; the window assertion proved nothing")
	}
	eventuallyLoaded(t, second, "window", "during-startup")
}

// TestLocalLRUEvictionDoesNotDeleteOrBroadcast pins eviction as a local
// concern: the evicted entry disappears from this map only, stays in Redis, and
// reaches peers.
func TestLocalLRUEvictionDoesNotDeleteOrBroadcast(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", MapOptions[testRecord]{
		Persist: PersistTTL,
		TTL:     time.Hour,
		MaxSize: 2,
	})
	second := newTestMap(t, server, "rules", persistentOptions())

	for index := range 3 {
		key := fmt.Sprintf("key-%d", index)
		if err := first.Set(context.Background(), key, testRecord{Name: key}); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}

	if _, ok := first.Load("key-0"); ok {
		t.Fatal("the least recently used entry was not evicted")
	}
	if held := first.Len(); held != 2 {
		t.Fatalf("local entries = %d, want 2 after eviction", held)
	}
	for index := range 3 {
		key := fmt.Sprintf("key-%d", index)
		eventuallyLoaded(t, second, key, key)
	}

	waitFor(t, 2*time.Second, func() bool {
		keys, err := server.HKeys(first.hashKey)
		return err == nil && len(keys) == 3
	}, "eviction removed an entry from Redis")

	// The peer keeps every key, which proves eviction broadcast no delete.
	time.Sleep(100 * time.Millisecond)
	if _, ok := second.Load("key-0"); !ok {
		t.Fatal("eviction was broadcast as a delete")
	}
}

// TestDecodeFailureLeavesNoLocalOrRemoteEntry covers both directions: a
// local write Decode rejects stores nothing anywhere, and a peer's value
// Decode rejects is skipped rather than stored raw.
func TestDecodeFailureLeavesNoLocalOrRemoteEntry(t *testing.T) {
	server := miniredis.RunT(t)
	var remoteAttempts atomic.Int64
	rejectingDecoder := func(key string, raw json.RawMessage) (testRecord, error) {
		if key == "bad" {
			remoteAttempts.Add(1)
			return testRecord{}, errors.New("rejected by test decoder")
		}
		return recordDecoder(key, raw)
	}

	first := newTestMap(t, server, "rules", MapOptions[testRecord]{
		Decode:  rejectingDecoder,
		Persist: PersistTTL,
		TTL:     time.Hour,
	})

	err := first.Set(context.Background(), "bad", testRecord{Name: "local"})
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("Set error = %v, want ErrDecode", err)
	}
	if _, ok := first.Load("bad"); ok {
		t.Fatal("a decode failure left a local entry")
	}
	if server.Exists(first.hashKey) {
		t.Fatal("a decode failure persisted an entry")
	}

	second := newTestMap(t, server, "rules", plainOptions())
	if err := second.Set(context.Background(), "bad", testRecord{Name: "remote"}); err != nil {
		t.Fatalf("peer Set: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return remoteAttempts.Load() >= 2 },
		"the first map never attempted the peer's value")
	time.Sleep(100 * time.Millisecond)
	if _, ok := first.Load("bad"); ok {
		t.Fatal("a value Decode rejected was stored locally")
	}
}

// TestPersistenceFailureLeavesLocalStateUnchanged pins the chosen write
// semantics: Redis is written before the local map, so a store failure changes
// nothing locally.
func TestPersistenceFailureLeavesLocalStateUnchanged(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Set(context.Background(), "kept", testRecord{Name: "kept"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := first.Set(ctx, "lost", testRecord{Name: "lost"})
	if !errors.Is(err, ErrPersist) {
		t.Fatalf("Set after store failure = %v, want ErrPersist", err)
	}
	if _, ok := first.Load("lost"); ok {
		t.Fatal("a failed persistence still changed the local map")
	}
	if _, ok := first.Load("kept"); !ok {
		t.Fatal("a failed persistence disturbed an existing local entry")
	}
	if err := first.Delete(ctx, "kept"); !errors.Is(err, ErrPersist) {
		t.Fatalf("Delete after store failure = %v, want ErrPersist", err)
	}
	if _, ok := first.Load("kept"); !ok {
		t.Fatal("a failed delete persistence still changed the local map")
	}
	if err := first.Clear(ctx); !errors.Is(err, ErrPersist) {
		t.Fatalf("Clear after store failure = %v, want ErrPersist", err)
	}
	if _, ok := first.Load("kept"); !ok {
		t.Fatal("a failed clear persistence still changed the local map")
	}
}

// TestBroadcastFailureReportsErrorAfterLocalChange pins the other half of the
// documented contract: once persistence and the local change have succeeded, a
// failed publication is reported but not rolled back.
func TestBroadcastFailureReportsErrorAfterLocalChange(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())

	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := first.Set(ctx, "local", testRecord{Name: "local"})
	if !errors.Is(err, ErrBroadcast) {
		t.Fatalf("Set after broadcast failure = %v, want ErrBroadcast", err)
	}
	if got, ok := first.Load("local"); !ok {
		t.Fatal("the documented semantics are local state already updated; it was not")
	} else if got.Name != "local" {
		t.Fatalf("local value = %#v", got)
	}

	if err := first.Delete(ctx, "local"); !errors.Is(err, ErrBroadcast) {
		t.Fatalf("Delete after broadcast failure = %v, want ErrBroadcast", err)
	}
	if _, ok := first.Load("local"); ok {
		t.Fatal("the documented semantics are local state already updated; Delete did not remove it")
	}

	if err := first.Set(ctx, "another", testRecord{Name: "another"}); !errors.Is(err, ErrBroadcast) {
		t.Fatalf("Set after broadcast failure = %v, want ErrBroadcast", err)
	}
	if err := first.Clear(ctx); !errors.Is(err, ErrBroadcast) {
		t.Fatalf("Clear after broadcast failure = %v, want ErrBroadcast", err)
	}
	if _, ok := first.Load("another"); ok {
		t.Fatal("the documented semantics are local state already updated; Clear did not run")
	}
}

func TestResyncRestoresPersistedSnapshot(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Set(context.Background(), "kept", testRecord{Name: "kept"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Simulate the state a disconnected replica can be left in: the entry is
	// gone locally while Redis still holds it.
	first.store.Clear()
	if _, ok := first.Load("kept"); ok {
		t.Fatal("test setup failed to clear the local map")
	}
	if err := first.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if value, ok := first.Load("kept"); !ok || value.Name != "kept" {
		t.Fatalf("Resync did not restore the snapshot: %#v, %t", value, ok)
	}
}

func TestResyncIsUnavailableWithoutPersistence(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())

	if err := first.Resync(context.Background()); !errors.Is(err, ErrPersistRequired) {
		t.Fatalf("Resync = %v, want ErrPersistRequired", err)
	}
}

func TestCloseStopsSubscriptionAndIsIdempotent(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-first.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close returned but the subscription loop did not stop")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
}

// TestCloseKeepsPersistedData pins Close as a local teardown: another replica
// that starts afterwards still finds the snapshot.
func TestCloseKeepsPersistedData(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Set(context.Background(), "kept", testRecord{Name: "kept"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := newTestMap(t, server, "rules", persistentOptions())
	if value, ok := second.Load("kept"); !ok || value.Name != "kept" {
		t.Fatalf("Close removed persisted data: %#v, %t", value, ok)
	}
}

func TestWritesAfterCloseReportErrClosed(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := first.Set(context.Background(), "key", testRecord{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close = %v, want ErrClosed", err)
	}
	if err := first.Delete(context.Background(), "key"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after Close = %v, want ErrClosed", err)
	}
	if err := first.Clear(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Clear after Close = %v, want ErrClosed", err)
	}
	if err := first.Resync(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Resync after Close = %v, want ErrClosed", err)
	}
}

// TestTwoInstancesOfTheSameNameEachWork exercises the documented absence of a
// process-wide registry: both instances subscribe, both observe the other's
// write, and neither is refused for reusing the name.
func TestTwoInstancesOfTheSameNameEachWork(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())
	second := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	eventuallyLoaded(t, second, "one", "alpha")

	if err := second.Set(context.Background(), "two", testRecord{Name: "beta"}); err != nil {
		t.Fatalf("peer Set: %v", err)
	}
	eventuallyLoaded(t, first, "two", "beta")
}

// TestTTLAppliesToTheStoredCollection pins the collection-wide expiry: every
// write refreshes the hash's TTL, and there is no per-entry expiry to observe.
func TestTTLAppliesToTheStoredCollection(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", persistentOptions())

	if err := first.Set(context.Background(), "one", testRecord{Name: "one"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := server.TTL(first.hashKey); got != time.Hour {
		t.Fatalf("collection TTL = %s, want 1h", got)
	}

	server.FastForward(30 * time.Minute)
	if got := server.TTL(first.hashKey); got != 30*time.Minute {
		t.Fatalf("collection TTL after 30m = %s, want 30m", got)
	}
	if err := first.Set(context.Background(), "two", testRecord{Name: "two"}); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if got := server.TTL(first.hashKey); got != time.Hour {
		t.Fatalf("collection TTL after a write = %s, want 1h: a write must refresh the whole collection", got)
	}
}

// TestSetAcceptsASourceTypeDifferentFromV proves Set's input need not equal
// the map's value type: Decode, not the writer's Go type, decides what readers
// see.
func TestSetAcceptsASourceTypeDifferentFromV(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())
	second := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "one", writeRecord{Name: "alpha", Count: 2}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	local, ok := first.Load("one")
	if !ok {
		t.Fatal("the writer did not decode its own write")
	}
	if local.Name != "alpha" || local.Count != 2 {
		t.Fatalf("local value = %#v, want a testRecord decoded from the writeRecord", local)
	}
	eventuallyLoaded(t, second, "one", "alpha")
}

// TestCustomDecodeRunsOnEveryReadPath pins the projection as shared: the local
// write and a peer notification both go through Decode, so readers never see
// two value shapes.
func TestCustomDecodeRunsOnEveryReadPath(t *testing.T) {
	server := miniredis.RunT(t)
	upper := func(key string, raw json.RawMessage) (testRecord, error) {
		record, err := recordDecoder(key, raw)
		if err != nil {
			return testRecord{}, err
		}
		record.Name = strings.ToUpper(record.Name)
		return record, nil
	}
	first := newTestMap(t, server, "rules", MapOptions[testRecord]{Decode: upper})
	second := newTestMap(t, server, "rules", MapOptions[testRecord]{Decode: upper})

	if err := first.Set(context.Background(), "one", testRecord{Name: "alpha"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if value, ok := first.Load("one"); !ok || value.Name != "ALPHA" {
		t.Fatalf("local value = %#v, %t; Decode did not run on the writer", value, ok)
	}
	eventuallyLoaded(t, second, "one", "ALPHA")
}

func TestEmptyKeysAreRejected(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestMap(t, server, "rules", plainOptions())

	if err := first.Set(context.Background(), "", testRecord{}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Set with an empty key = %v, want ErrInvalidKey", err)
	}
	if err := first.Delete(context.Background(), ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Delete with an empty key = %v, want ErrInvalidKey", err)
	}
}
