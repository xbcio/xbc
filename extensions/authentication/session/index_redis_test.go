package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var errInjected = errors.New("injected Redis failure")

// commandHook lets a test fail a command before it reaches the server (fail)
// or observe one after it succeeded (after). Both run on the calling goroutine.
type commandHook struct {
	fail  func(name string, args []any) bool
	after func(cmd goredis.Cmder)
}

func (h *commandHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *commandHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if h.fail != nil && h.fail(cmd.Name(), cmd.Args()) {
			return errInjected
		}
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if h.after != nil {
			h.after(cmd)
		}
		return nil
	}
}

func (h *commandHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// scriptKeyOf returns the first key of an EVAL-family command and "" for any
// other command: the arguments are the name, the script or its SHA, the key
// count, then the keys.
func scriptKeyOf(name string, args []any) string {
	if (name != "evalsha" && name != "eval") || len(args) < 4 {
		return ""
	}
	key, _ := args[3].(string)
	return key
}

func failIndexScripts(store *RedisStore) func(string, []any) bool {
	return func(name string, args []any) bool {
		return strings.HasPrefix(scriptKeyOf(name, args), store.prefix+"subject:")
	}
}

func failSessionScripts(store *RedisStore) func(string, []any) bool {
	return func(name string, args []any) bool {
		key := scriptKeyOf(name, args)
		return key != "" && !strings.HasPrefix(key, store.prefix+"subject:")
	}
}

// Conformance 12: an index write that fails fails the Create as a whole and
// leaves no session behind -- the index is written before the session.
func TestRedisStoreCreateWithAFailedIndexStepStoresNoSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	hook := &commandHook{}
	client.AddHook(hook)
	hook.fail = failIndexScripts(store)
	ctx := context.Background()

	if err := store.Create(ctx, sessionValue(testID(1, 32), "alice", now, time.Hour), 30*time.Minute); !errors.Is(err, errInjected) {
		t.Fatalf("store Create = %v, want the injected index failure", err)
	}
	if _, err := testManager(t, store, now).Create(ctx, "alice", nil); !errors.Is(err, errInjected) {
		t.Fatalf("manager Create = %v, want the injected index failure", err)
	}
	if keys := server.Keys(); len(keys) != 0 {
		t.Fatalf("a Create whose index step failed left keys behind: %v", keys)
	}
}

// A session write that fails after the index step takes its index entry back
// instead of leaving a member that names nothing.
func TestRedisStoreCreateWithAFailedSessionWriteRollsTheIndexBack(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	hook := &commandHook{}
	client.AddHook(hook)
	hook.fail = failSessionScripts(store)

	err := store.Create(context.Background(), sessionValue(testID(2, 32), "alice", now, time.Hour), 30*time.Minute)
	if !errors.Is(err, errInjected) {
		t.Fatalf("Create = %v, want the injected session failure", err)
	}
	if keys := server.Keys(); len(keys) != 0 {
		t.Fatalf("a failed session write left keys behind: %v", keys)
	}
}

// A rotation whose index step fails changes nothing: the old session stays and
// the replacement is never stored.
func TestRedisStoreRotateWithAFailedIndexStepLeavesTheOldSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	hook := &commandHook{}
	client.AddHook(hook)
	ctx := context.Background()
	oldID, replacementID := testID(3, 32), testID(4, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	hook.fail = failIndexScripts(store)
	if err := store.Rotate(ctx, oldID, sessionValue(replacementID, "alice", now, time.Hour), 30*time.Minute); !errors.Is(err, errInjected) {
		t.Fatalf("Rotate = %v, want the injected index failure", err)
	}
	if !server.Exists(store.key(oldID)) || server.Exists(store.key(replacementID)) {
		t.Fatal("a failed index step must leave the old session and store no replacement")
	}
	if members, _ := server.SMembers(store.subjectKey("alice")); !slices.Equal(members, []string{IDDigest(oldID)}) {
		t.Fatalf("index = %v, want only the old digest", members)
	}
}

// Delete removes the session before its index entry: when the session delete
// fails, the entry must still be there, or a live session would be unindexed.
func TestRedisStoreDeleteKeepsTheIndexEntryWhenTheSessionDeleteFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	hook := &commandHook{}
	client.AddHook(hook)
	ctx := context.Background()
	id := testID(5, 32)
	if err := store.Create(ctx, sessionValue(id, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	hook.fail = failSessionScripts(store)
	if err := store.Delete(ctx, id); !errors.Is(err, errInjected) {
		t.Fatalf("Delete = %v, want the injected failure", err)
	}
	if !server.Exists(store.key(id)) {
		t.Fatal("the session vanished although its delete failed")
	}
	if members, _ := server.SMembers(store.subjectKey("alice")); !slices.Equal(members, []string{IDDigest(id)}) {
		t.Fatalf("index = %v; a live session must stay indexed", members)
	}

	hook.fail = nil
	if err := store.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if server.Exists(store.key(id)) || server.Exists(store.subjectKey("alice")) {
		t.Fatal("a successful Delete left the session or its index entry behind")
	}
}

// Conformance 8: the subject set's TTL only grows. A short-lived member never
// shortens the lease a long-lived one needs, and the lease follows time down
// until a longer-lived member lifts it again.
func TestRedisStoreSubjectSetTTLOnlyExtends(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	setKey := store.subjectKey("alice")
	create := func(seed byte, lifetime time.Duration) {
		t.Helper()
		if err := store.Create(ctx, sessionValue(testID(seed, 32), "alice", now, lifetime), 30*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	advance := func(d time.Duration) {
		server.FastForward(d)
		now = now.Add(d)
	}

	create(10, 2*time.Hour)
	if ttl := server.TTL(setKey); ttl != 2*time.Hour {
		t.Fatalf("TTL after the first Create = %v, want 2h", ttl)
	}
	create(11, 10*time.Minute)
	if ttl := server.TTL(setKey); ttl != 2*time.Hour {
		t.Fatalf("a shorter-lived member shortened the set TTL to %v", ttl)
	}
	advance(30 * time.Minute)
	create(12, time.Hour)
	if ttl := server.TTL(setKey); ttl != 90*time.Minute {
		t.Fatalf("TTL = %v, want the 90m the first member still needs", ttl)
	}
	create(13, 3*time.Hour)
	if ttl := server.TTL(setKey); ttl != 3*time.Hour {
		t.Fatalf("TTL = %v, want it extended to 3h by the longest member", ttl)
	}
}

// A replacement with no lifetime left is not stored, so it must not be indexed:
// the set ends empty rather than naming a session that can never exist.
func TestRedisStoreRotateIntoAnExpiredReplacementIndexesNothing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	oldID := testID(20, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	expired := sessionValue(testID(21, 32), "alice", now.Add(-2*time.Hour), time.Hour)
	if err := store.Rotate(ctx, oldID, expired, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if members, _ := server.SMembers(store.subjectKey("alice")); len(members) != 0 {
		t.Fatalf("index = %v, want empty: the expired replacement must not be indexed", members)
	}
}

// RevokeBySubject removes the members it read and nothing else. A session that
// is created after the read but before the removal has to stay indexed, which
// deleting the whole set would break.
func TestRedisStoreRevokeBySubjectKeepsAMemberAddedWhileItRuns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	earlyID, lateID := testID(30, 32), testID(31, 32)
	if err := store.Create(ctx, sessionValue(earlyID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	var armed atomic.Bool
	armed.Store(true)
	var createErr error
	client.AddHook(&commandHook{after: func(cmd goredis.Cmder) {
		if cmd.Name() == "smembers" && armed.CompareAndSwap(true, false) {
			createErr = store.Create(ctx, sessionValue(lateID, "alice", now, time.Hour), 30*time.Minute)
		}
	}})

	revoked, err := store.RevokeBySubject(ctx, "alice")
	if err != nil || createErr != nil {
		t.Fatalf("RevokeBySubject = %d, %v; late Create = %v", revoked, err, createErr)
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d, want only the session that was listed when the revoke read the set", revoked)
	}
	if server.Exists(store.key(earlyID)) || !server.Exists(store.key(lateID)) {
		t.Fatal("the listed session must be gone and the late one must survive")
	}
	if members, _ := server.SMembers(store.subjectKey("alice")); !slices.Equal(members, []string{IDDigest(lateID)}) {
		t.Fatalf("index = %v; the late session must stay indexed", members)
	}
}

// Every command the index issues -- scripts, set commands, pipelined reads and
// deletes -- addresses exactly one key, which is what a cluster client needs.
func TestRedisStoreIndexOperationsStaySingleKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	recorder := &commandRecorder{}
	client.AddHook(recorder)

	// Open the connection first: its HELLO handshake is not an index command.
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	recorder.start()
	firstID, secondID, thirdID := testID(40, 32), testID(41, 32), testID(42, 32)
	steps := []func() error{
		func() error { return store.Create(ctx, sessionValue(firstID, "alice", now, time.Hour), 30*time.Minute) },
		func() error {
			return store.Create(ctx, sessionValue(secondID, "alice", now, time.Hour), 30*time.Minute)
		},
		func() error {
			return store.Rotate(ctx, secondID, sessionValue(thirdID, "alice", now, time.Hour), 30*time.Minute)
		},
		func() error { _, err := store.ListBySubject(ctx, "alice"); return err },
		func() error { _, err := store.RevokeByDigest(ctx, "alice", IDDigest(thirdID)); return err },
		func() error { return store.Delete(ctx, firstID) },
		func() error { _, err := store.RevokeBySubject(ctx, "alice"); return err },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	records := recorder.stop()

	seen := map[string]bool{}
	for _, record := range records {
		seen[record.name] = true
		if keys := recordedKeys(t, record); len(keys) != 1 {
			t.Fatalf("%s addressed %d keys: %v", record.name, len(keys), keys)
		}
	}
	for _, want := range []string{"smembers", "sismember", "srem", "hmget", "hget", "del"} {
		if !seen[want] {
			t.Fatalf("the audit never saw %q, so it did not cover that path; saw %v", want, recordedNames(records))
		}
	}
}

// A session whose Redis key is still present but whose logical ExpiresAt has
// passed under the store's clock is not live: ListBySubject must omit it and
// prune its index member, while a still-valid session of the same subject stays
// listed. Redis time is deliberately not fast-forwarded, so only the logical
// check can tell the two apart.
func TestRedisStoreListBySubjectOmitsASessionPastItsExpiresAtWhoseKeyRemains(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	pastID, liveID := testID(40, 32), testID(41, 32)
	if err := store.Create(ctx, sessionValue(pastID, "alice", now, 10*time.Minute), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, sessionValue(liveID, "alice", now, 2*time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	now = now.Add(20 * time.Minute)
	if !server.Exists(store.key(pastID)) {
		t.Fatal("the premise failed: the past-ExpiresAt session key must still exist in Redis")
	}
	infos, err := store.ListBySubject(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Digest != IDDigest(liveID) {
		t.Fatalf("ListBySubject = %v, want only the still-valid session %s", infos, IDDigest(liveID))
	}
	if members, _ := server.SMembers(store.subjectKey("alice")); !slices.Equal(members, []string{IDDigest(liveID)}) {
		t.Fatalf("index = %v, want only the live member after pruning", members)
	}
}
