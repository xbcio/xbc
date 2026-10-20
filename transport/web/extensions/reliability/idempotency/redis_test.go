package idempotency

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

// TestRedisStorePrefixIsolatesNamespaces proves redis_prefix separates two
// stores over the same server: a record completed under one prefix is invisible
// to a store configured with another, which acquires the same key and
// fingerprint fresh instead of replaying the first store's response.
func TestRedisStorePrefixIsolatesNamespaces(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	first, err := NewRedisStore(client, "first:idempotency:")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRedisStore(client, "second:idempotency:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if acquired, err := first.Acquire(ctx, "key", "fp", "owner-a", time.Minute); err != nil || acquired.State != Acquired {
		t.Fatalf("first acquire = %#v, %v", acquired, err)
	}
	stored := Response{Status: 201, ContentType: "application/json", Body: []byte(`{"owner":"first"}`)}
	if err := first.Complete(ctx, "key", "fp", "owner-a", stored, time.Hour); err != nil {
		t.Fatal(err)
	}
	isolated, err := second.Acquire(ctx, "key", "fp", "owner-b", time.Minute)
	if err != nil || isolated.State != Acquired {
		t.Fatalf("second prefix acquire = %#v, %v, want Acquired", isolated, err)
	}
	if isolated.Response.Status != 0 || len(isolated.Response.Body) != 0 {
		t.Fatalf("second prefix observed the first prefix's response: %#v", isolated.Response)
	}
	replayed, err := first.Acquire(ctx, "key", "fp", "owner-c", time.Minute)
	if err != nil || replayed.State != Completed || string(replayed.Response.Body) != string(stored.Body) {
		t.Fatalf("first prefix replay = %#v, %v", replayed, err)
	}
}

func TestRedisStorePersistsReplayHeaders(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisStore(client, "test:idempotency:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Acquire(ctx, "key", "fp", "owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Location", "/orders/9")
	header.Set("ETag", `"v9"`)
	header.Set("Content-Disposition", `attachment; filename="a b.json"`)
	header.Add("Cache-Control", "no-store")
	header.Add("Cache-Control", "private")
	response := Response{Status: 201, ContentType: "application/json", Header: header, Body: []byte(`{"id":9}`)}
	if err := store.Complete(ctx, "key", "fp", "owner", response, time.Hour); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Acquire(ctx, "key", "fp", "other", time.Minute)
	if err != nil || replay.State != Completed {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if got := replay.Response.Header.Get("Location"); got != "/orders/9" {
		t.Fatalf("Location = %q", got)
	}
	if got := replay.Response.Header.Get("ETag"); got != `"v9"` {
		t.Fatalf("ETag = %q", got)
	}
	if got := replay.Response.Header.Get("Content-Disposition"); got != `attachment; filename="a b.json"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := replay.Response.Header.Values("Cache-Control"); !slices.Equal(got, []string{"no-store", "private"}) {
		t.Fatalf("Cache-Control = %q", got)
	}
	replay.Response.Header.Set("Location", "/mutated")
	again, _ := store.Acquire(ctx, "key", "fp", "third", time.Minute)
	if got := again.Response.Header.Get("Location"); got != "/orders/9" {
		t.Fatalf("mutating a replay changed the record: Location = %q", got)
	}
}

// TestRedisStoreReplaysRecordWrittenBeforeHeaderPersistence locks the stated
// migration behaviour: a record written by a binary that predates the header
// whitelist carries no headers field and replays with its content type only.
func TestRedisStoreReplaysRecordWrittenBeforeHeaderPersistence(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := NewRedisStore(client, "test:idempotency:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := store.redisKey("legacy")
	if err := client.HSet(ctx, key, map[string]any{
		"state":        "completed",
		"fingerprint":  "fp",
		"status":       "201",
		"content_type": "application/json",
		"body":         `{"id":7}`,
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.PExpire(ctx, key, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	result, err := store.Acquire(ctx, "legacy", "fp", "owner", time.Minute)
	if err != nil || result.State != Completed {
		t.Fatalf("legacy record = %#v, %v", result, err)
	}
	if result.Response.Status != 201 || result.Response.ContentType != "application/json" || string(result.Response.Body) != `{"id":7}` {
		t.Fatalf("legacy response = %#v", result.Response)
	}
	if len(result.Response.Header) != 0 {
		t.Fatalf("legacy record gained headers: %#v", result.Response.Header)
	}
}

// TestParseCompletedReplyRejectsMalformedArity exercises the defensive arity
// guard the Lua script itself can never trip: a non-conforming server or proxy
// answering with four or six elements must produce an error, not an index
// panic.
func TestParseCompletedReplyRejectsMalformedArity(t *testing.T) {
	valid := []string{"completed", "201", "application/json", `{"id":1}`, ""}
	result, err := parseCompletedReply(valid)
	if err != nil || result.State != Completed || result.Response.Status != 201 {
		t.Fatalf("valid reply = %#v, %v", result, err)
	}
	short := []string{"completed", "201", "application/json", `{"id":1}`}
	if _, err := parseCompletedReply(short); err == nil {
		t.Fatal("four-element reply accepted")
	}
	long := append(append([]string(nil), valid...), "extra")
	if _, err := parseCompletedReply(long); err == nil {
		t.Fatal("six-element reply accepted")
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
