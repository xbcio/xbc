package session

import (
	"context"
	"errors"
	"strconv"
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

// commandRecorder records the commands a client sends. It sits on the client
// boundary rather than the server's because miniredis dispatches the redis.call
// invocations inside a Lua script through the same counter as client commands,
// so its CommandCount cannot say how many round trips a rotation costs.
type commandRecorder struct {
	mu        sync.Mutex
	recording bool
	records   []commandRecord
}

type commandRecord struct {
	name string
	args []any
}

func (r *commandRecorder) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (r *commandRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		r.mu.Lock()
		if r.recording {
			r.records = append(r.records, commandRecord{name: cmd.Name(), args: append([]any(nil), cmd.Args()...)})
		}
		r.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (r *commandRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func (r *commandRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = true
	r.records = nil
}

func (r *commandRecorder) stop() []commandRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	return append([]commandRecord(nil), r.records...)
}

// TestRedisStoreRotateRunsAsThreeSingleKeySteps pins the shape the cluster
// compatibility rests on: a successful rotation is three commands -- validate,
// install, delete -- and every one of them addresses exactly one key. The
// previous implementation passed both keys to a single EVAL, which a cluster
// refuses with CROSSSLOT; no single-node test topology can reproduce that
// refusal, so the invariant is pinned on the scripts' source and on the
// commands the client actually sends.
func TestRedisStoreRotateRunsAsThreeSingleKeySteps(t *testing.T) {
	for name, source := range map[string]string{
		"validate": redisRotateValidateSource,
		"install":  redisRotateInstallSource,
	} {
		if !strings.Contains(source, "KEYS[1]") {
			t.Fatalf("%s script does not address KEYS[1]", name)
		}
		if strings.Contains(source, "KEYS[2]") {
			t.Fatalf("%s script addresses a second key; rotation must stay single-key", name)
		}
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	_, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()

	recorder := &commandRecorder{}
	client.AddHook(recorder)

	// Warm the server's script cache first: the first run of a script costs an
	// extra round trip, EVALSHA missing and EVAL carrying the source.
	warmID := testID(31, 32)
	if err := store.Create(ctx, sessionValue(warmID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Rotate(ctx, warmID, sessionValue(testID(32, 32), "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	oldID := testID(33, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	replacementID := testID(34, 32)

	recorder.start()
	if err := store.Rotate(ctx, oldID, sessionValue(replacementID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	records := recorder.stop()

	wantNames := []string{"evalsha", "evalsha", "del"}
	if len(records) != len(wantNames) {
		t.Fatalf("rotation sent %v, want %v", recordedNames(records), wantNames)
	}
	for i, want := range wantNames {
		if records[i].name != want {
			t.Fatalf("rotation sent %v, want %v", recordedNames(records), wantNames)
		}
	}
	wantKeys := []string{store.key(oldID), store.key(replacementID), store.key(oldID)}
	for i, record := range records {
		keys := recordedKeys(t, record)
		if len(keys) != 1 {
			t.Fatalf("rotation step %d (%s) addressed %d keys: %v", i+1, record.name, len(keys), keys)
		}
		if keys[0] != wantKeys[i] {
			t.Fatalf("rotation step %d (%s) addressed %q, want %q", i+1, record.name, keys[0], wantKeys[i])
		}
	}
}

func recordedNames(records []commandRecord) []string {
	names := make([]string, len(records))
	for i, record := range records {
		names[i] = record.name
	}
	return names
}

// recordedKeys extracts the key arguments of a recorded command: an EVAL-family
// command carries the key count at index 2 followed by the keys, and DEL's
// arguments are all keys.
func recordedKeys(t *testing.T, record commandRecord) []string {
	t.Helper()
	switch record.name {
	case "evalsha", "eval":
		if len(record.args) < 3 {
			t.Fatalf("%s recorded with too few arguments: %v", record.name, record.args)
		}
		count, ok := record.args[2].(int)
		if !ok || count < 0 || 3+count > len(record.args) {
			t.Fatalf("%s recorded key count %v for %d arguments", record.name, record.args[2], len(record.args))
		}
		keys := make([]string, 0, count)
		for _, arg := range record.args[3 : 3+count] {
			text, ok := arg.(string)
			if !ok {
				t.Fatalf("%s key has type %T", record.name, arg)
			}
			keys = append(keys, text)
		}
		return keys
	case "del":
		keys := make([]string, 0, len(record.args)-1)
		for _, arg := range record.args[1:] {
			text, ok := arg.(string)
			if !ok {
				t.Fatalf("del key has type %T", arg)
			}
			keys = append(keys, text)
		}
		return keys
	default:
		t.Fatalf("unexpected command %q in a rotation", record.name)
		return nil
	}
}

// TestRedisStoreRotateUnderConcurrentCallers pins what the sequence still
// guarantees under concurrency, and records what it no longer does. Exactly one
// winner is not guaranteed anymore: several callers can pass the validation
// step before any of them deletes the old ID, and each installs its own
// replacement -- a replacement ID is only ever returned to the caller that
// minted it, so the extra ones belong to callers that asked for a rotation.
// What must hold is that the old ID never survives, that a nil return always
// corresponds to a live replacement, and that no other key appears.
func TestRedisStoreRotateUnderConcurrentCallers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	oldID := testID(35, 32)
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
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrAlreadyExists):
			default:
				t.Errorf("Rotate error = %v", err)
			}
		}(newIDs[i])
	}
	wg.Wait()

	if successes.Load() < 1 {
		t.Fatal("no caller succeeded, so the old ID could never be rotated away")
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
	if replacements != int(successes.Load()) {
		t.Fatalf("live replacements=%d, callers that succeeded=%d", replacements, successes.Load())
	}
	if keys := server.Keys(); len(keys) != replacements {
		t.Fatalf("Redis holds %d keys, want exactly one per replacement", len(keys))
	}
}

// TestRedisStoreRotateInstallStepIsGuarded covers the two outcomes of the
// install step the sequence's retry and conflict rules rest on.
func TestRedisStoreRotateInstallStepIsGuarded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()

	oldID := testID(36, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	// A replacement ID already taken by a different session is reported, and the
	// old ID stays in place so the caller may retry.
	takenID := testID(37, 32)
	if err := store.Create(ctx, sessionValue(takenID, "mallory", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Rotate(ctx, oldID, sessionValue(takenID, "alice", now, time.Hour), 30*time.Minute); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Rotate onto a taken ID error = %v, want ErrAlreadyExists", err)
	}
	if _, found, _ := store.Get(ctx, oldID); !found {
		t.Fatal("a refused install must leave the old ID in place")
	}
	if got, _, _ := store.Get(ctx, takenID); got.Subject != "mallory" {
		t.Fatalf("the taken ID now belongs to %q; a conflict must not overwrite it", got.Subject)
	}

	// A retry that finds its own replacement already installed -- the install
	// completed but the delete did not -- finishes the rotation instead of
	// reporting a conflict.
	replacementID := testID(38, 32)
	replacement := sessionValue(replacementID, "alice", now, time.Hour)
	if err := store.Create(ctx, replacement, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Rotate(ctx, oldID, replacement, 30*time.Minute); err != nil {
		t.Fatalf("Rotate whose replacement is already installed error = %v", err)
	}
	if _, found, _ := store.Get(ctx, oldID); found {
		t.Fatal("old ID survived the completing retry")
	}
	if _, found, _ := store.Get(ctx, replacementID); !found {
		t.Fatal("the completing retry removed its own replacement")
	}
}

// TestRedisStoreRotateFailsSafeAroundTheOldID covers the validation step's two
// not-found outcomes: an old ID that does not exist installs nothing, and an
// old ID whose absolute lifetime has passed is removed rather than rotated.
func TestRedisStoreRotateFailsSafeAroundTheOldID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()

	oldID := testID(39, 32)
	if err := store.Rotate(ctx, oldID, sessionValue(testID(40, 32), "alice", now, time.Hour), 30*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rotate(missing) error = %v, want ErrNotFound", err)
	}
	if keys := server.Keys(); len(keys) != 0 {
		t.Fatalf("a refused rotation created keys: %v", keys)
	}

	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	// The absolute field is rewritten directly: the public Create always leases
	// the key until at least its absolute expiry, so only the direct write can
	// model "the absolute lifetime passed while the key is still in Redis".
	key := store.key(oldID)
	server.HSet(key, "absolute", strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10))
	if err := store.Rotate(ctx, oldID, sessionValue(testID(41, 32), "alice", now, time.Hour), 30*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rotate(expired) error = %v, want ErrNotFound", err)
	}
	if server.Exists(key) {
		t.Fatal("the expired old ID survived the failed rotation")
	}
}

// TestRedisStoreRotateWithAnExpiredReplacementRetiresTheOldID pins the edge the
// install step leaves open. A replacement already past its absolute lifetime
// gets a non-positive lease, so nothing usable survives the sequence -- but the
// sequence still reports success and still retires the old ID, which is what
// MemoryStore does with such a replacement: store it for Get to prune. The
// caller ends up without a session either way, which Manager surfaces as an
// ErrInvalidCookie from SetCookie.
func TestRedisStoreRotateWithAnExpiredReplacementRetiresTheOldID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()

	oldID := testID(44, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	replacementID := testID(45, 32)
	expired := sessionValue(replacementID, "alice", now.Add(-2*time.Hour), time.Hour)
	if err := store.Rotate(ctx, oldID, expired, 30*time.Minute); err != nil {
		t.Fatalf("Rotate(expired replacement) error = %v", err)
	}
	if _, found, _ := store.Get(ctx, oldID); found {
		t.Fatal("old ID survived rotation into an expired replacement")
	}
	if _, found, _ := store.Get(ctx, replacementID); found {
		t.Fatal("an expired replacement resolved after rotation")
	}
}

// TestRedisStoreRotateInstallsTheReplacementWithTheTrimmedLease pins that the
// install step caps the replacement's idle lease at the session's remaining
// absolute lifetime, as the single-script version did.
func TestRedisStoreRotateInstallsTheReplacementWithTheTrimmedLease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()

	oldID := testID(42, 32)
	if err := store.Create(ctx, sessionValue(oldID, "alice", now, 40*time.Second), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	replacementID := testID(43, 32)
	if err := store.Rotate(ctx, oldID, sessionValue(replacementID, "alice", now, 40*time.Second), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := server.TTL(store.key(replacementID)); ttl != 40*time.Second {
		t.Fatalf("replacement TTL = %s, want the remaining absolute lifetime %s", ttl, 40*time.Second)
	}
}

func TestRedisStoreValidatesConstructionAndContext(t *testing.T) {
	if _, err := NewRedisStore(nil, "prefix:"); err == nil {
		t.Fatal("nil Redis client accepted")
	}
	// NewRedisStore takes the topology-neutral interface, so a typed nil must
	// be rejected exactly like an untyped one.
	var typedNil *goredis.Client
	if _, err := NewRedisStore(typedNil, "prefix:"); err == nil {
		t.Fatal("typed-nil Redis client accepted")
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
