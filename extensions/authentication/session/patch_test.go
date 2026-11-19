package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// patchStore exposes the PatchStore capability of a bare backend harness.
func (h *indexHarness) patchStore(t *testing.T) PatchStore {
	t.Helper()
	store, ok := h.store.(PatchStore)
	if !ok {
		t.Fatalf("backend store %T does not implement PatchStore", h.store)
	}
	return store
}

// Patch applies a delta in place on both built-in backends: unset keys go, set
// keys arrive, and identity, times, and untouched attributes are preserved.
func TestPatchStoresApplyDeltaInPlaceAndPreserveIdentity(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		id, digest := h.create(t, 90, "alice", time.Hour)
		before, found, err := h.store.Get(ctx, id)
		if err != nil || !found {
			t.Fatalf("Get() found=%v err=%v", found, err)
		}

		input := map[string]any{"role": "reader", "extra": true}
		patched, ok, err := h.patchStore(t).PatchAttributes(ctx, id, input, []string{"nested"})
		if err != nil || !ok {
			t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
		}
		if patched.ID != id || patched.Subject != before.Subject ||
			!patched.IssuedAt.Equal(before.IssuedAt) || !patched.ExpiresAt.Equal(before.ExpiresAt) {
			t.Fatalf("patched identity = %#v, want ID/subject/times from %#v", patched, before)
		}
		if patched.Attributes["role"] != "reader" || patched.Attributes["extra"] != true {
			t.Fatalf("patched attributes = %#v", patched.Attributes)
		}
		if _, present := patched.Attributes["nested"]; present {
			t.Fatalf("unset key survived: %#v", patched.Attributes)
		}

		// Both directions are defensive: neither the input map nor a result map
		// may reach the stored value.
		input["role"] = "mutated"
		patched.Attributes["role"] = "mutated"
		stored, found, err := h.store.Get(ctx, id)
		if err != nil || !found {
			t.Fatalf("Get() after patch found=%v err=%v", found, err)
		}
		if stored.Attributes["role"] != "reader" {
			t.Fatalf("stored attributes were aliased: %#v", stored.Attributes)
		}
		if _, present := stored.Attributes["nested"]; present {
			t.Fatalf("unset key came back: %#v", stored.Attributes)
		}

		infos, err := h.store.ListBySubject(ctx, "alice")
		if err != nil || len(infos) != 1 || infos[0].Digest != digest {
			t.Fatalf("List() = %v, %v; want the same indexed session", infos, err)
		}
		if infos[0].Attributes["role"] != "reader" {
			t.Fatalf("List() attributes = %#v, want the patched value", infos[0].Attributes)
		}
	})
}

// An empty patch is a lookup: it returns the current session, leaves nil
// attributes nil, and an unset-only patch of absent keys changes nothing. Utterly
// empty patches are accepted on a live session and still refuse a dead one.
func TestPatchStoresHandleEmptyPatchesAndNilAttributes(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		store := h.patchStore(t)
		id := testID(91, 32)
		value := Session{
			ID:        id,
			Subject:   "alice",
			IssuedAt:  *h.now,
			ExpiresAt: h.now.Add(time.Hour),
		}
		if err := h.store.Create(ctx, value, 30*time.Minute); err != nil {
			t.Fatal(err)
		}

		current, ok, err := store.PatchAttributes(ctx, id, nil, nil)
		if err != nil || !ok {
			t.Fatalf("empty PatchAttributes() ok=%v err=%v", ok, err)
		}
		if current.Attributes != nil {
			t.Fatalf("an empty patch materialized nil attributes: %#v", current.Attributes)
		}
		again, ok, err := store.PatchAttributes(ctx, id, nil, []string{"absent"})
		if err != nil || !ok {
			t.Fatalf("unset-only PatchAttributes() ok=%v err=%v", ok, err)
		}
		if again.Attributes != nil {
			t.Fatalf("unsetting an absent key from nil attributes materialized them: %#v", again.Attributes)
		}

		seeded, seedDigest := h.create(t, 92, "alice", time.Hour)
		unchanged, ok, err := store.PatchAttributes(ctx, seeded, nil, nil)
		if err != nil || !ok {
			t.Fatalf("empty PatchAttributes() on seeded session ok=%v err=%v", ok, err)
		}
		if unchanged.Attributes["role"] != "admin" || unchanged.Attributes["nested"].(map[string]any)["team"] != "core" {
			t.Fatalf("empty patch changed attributes: %#v", unchanged.Attributes)
		}
		if members := h.members("alice"); !containsAll(members, seedDigest) {
			t.Fatalf("index after empty patches = %v, want it to still name %s", members, seedDigest[:8])
		}

		removed, ok, err := store.PatchAttributes(ctx, seeded, nil, []string{"role"})
		if err != nil || !ok {
			t.Fatalf("unset-only PatchAttributes() ok=%v err=%v", ok, err)
		}
		if _, present := removed.Attributes["role"]; present {
			t.Fatalf("unset key survived: %#v", removed.Attributes)
		}

		nulled, ok, err := store.PatchAttributes(ctx, seeded, map[string]any{"maybe": nil}, nil)
		if err != nil || !ok {
			t.Fatalf("null-valued PatchAttributes() ok=%v err=%v", ok, err)
		}
		if nullValue, present := nulled.Attributes["maybe"]; !present || nullValue != nil {
			t.Fatalf("JSON null was not stored as a null value: %#v", nulled.Attributes)
		}
	})
}

func containsAll(values []string, wanted ...string) bool {
	for _, want := range wanted {
		found := false
		for _, value := range values {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// A key named on both sides, non-JSON-safe set values, an empty ID, and a nil
// context are parameter errors that change nothing.
func TestPatchStoresRejectConflictingAndInvalidArguments(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		store := h.patchStore(t)
		id, _ := h.create(t, 93, "alice", time.Hour)

		if _, _, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, []string{"role"}); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("conflicting set/unset = %v, want a parameter error", err)
		}
		if _, _, err := store.PatchAttributes(ctx, id, map[string]any{"bad": make(chan int)}, nil); err == nil {
			t.Fatal("PatchAttributes accepted a non-JSON set value")
		}
		if _, _, err := store.PatchAttributes(nil, id, nil, nil); err == nil {
			t.Fatal("PatchAttributes accepted a nil context")
		}
		if _, _, err := store.PatchAttributes(ctx, "", nil, nil); err == nil {
			t.Fatal("PatchAttributes accepted an empty ID")
		}

		stored, found, err := h.store.Get(ctx, id)
		if err != nil || !found || stored.Attributes["role"] != "admin" {
			t.Fatalf("a refused patch changed the session: %#v, %v, %v", stored.Attributes, found, err)
		}
	})
}

// Every dead session is refused identically, and a refused patch never creates
// a record or an index member.
func TestPatchStoresRefuseExpiredRevokedAndUnknownSessions(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		store := h.patchStore(t)

		expiredID, _ := h.create(t, 94, "alice", time.Minute)
		revokedID, _ := h.create(t, 95, "alice", time.Hour)
		if err := h.store.Delete(ctx, revokedID); err != nil {
			t.Fatal(err)
		}
		h.advance(2 * time.Minute)
		membersBefore := h.members("alice")

		for name, id := range map[string]string{
			"expired":       expiredID,
			"revoked":       revokedID,
			"never created": testID(96, 32),
		} {
			value, found, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, nil)
			if err != nil || found {
				t.Fatalf("%s PatchAttributes() = %#v, %v, %v; want not found", name, value, found, err)
			}
		}
		if members := h.members("alice"); !subsetOf(members, membersBefore) {
			t.Fatalf("index = %v, want no member beyond the pre-patch %v", members, membersBefore)
		}
		if h.liveTotal() != 0 {
			t.Fatalf("%d sessions exist; a refused patch must not create one", h.liveTotal())
		}
	})
}

// The manager applies the delta, keeps the ID, subject, and both timestamps,
// and surfaces ErrNotFound for every dead session and ErrStopped after Stop.
func TestManagerPatchAttributesPreservesLifetimeAndRefusesDeadSessions(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)
		created, err := m.Create(ctx, "alice", map[string]any{"role": "admin", "nested": map[string]any{"team": "core"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.PatchAttributes(ctx, created.ID, map[string]any{"bad": make(chan int)}, nil); err == nil {
			t.Fatal("PatchAttributes accepted a non-JSON set value")
		}
		if _, err := m.PatchAttributes(ctx, created.ID, map[string]any{"role": "x"}, []string{"role"}); err == nil {
			t.Fatal("PatchAttributes accepted the same key in set and unset")
		}
		if _, err := m.PatchAttributes(ctx, "attacker-selected", nil, nil); !errors.Is(err, ErrInvalidCookie) {
			t.Fatalf("PatchAttributes(malformed ID) = %v, want ErrInvalidCookie", err)
		}
		if _, err := m.PatchAttributes(ctx, testID(97, 32), nil, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PatchAttributes(unknown ID) = %v, want ErrNotFound", err)
		}

		patched, err := m.PatchAttributes(ctx, created.ID, map[string]any{"role": "reader", "extra": true}, []string{"nested"})
		if err != nil {
			t.Fatal(err)
		}
		if patched.ID != created.ID || patched.Subject != created.Subject ||
			!patched.IssuedAt.Equal(created.IssuedAt) || !patched.ExpiresAt.Equal(created.ExpiresAt) {
			t.Fatalf("patched = %#v, want identity and lifetime of %#v", patched, created)
		}
		if patched.Attributes["role"] != "reader" || patched.Attributes["extra"] != true {
			t.Fatalf("patched attributes = %#v", patched.Attributes)
		}
		if _, present := patched.Attributes["nested"]; present {
			t.Fatalf("unset key survived: %#v", patched.Attributes)
		}
		value, verdict, err := m.Verify(ctx, created.ID)
		if err != nil || verdict != VerdictValid || value.Attributes["role"] != "reader" {
			t.Fatalf("Verify() = %#v, %v, %v; want the patched session", value, verdict, err)
		}

		if _, err := m.PatchAttributes(ctx, created.ID, nil, nil); err != nil {
			t.Fatalf("empty PatchAttributes() = %v", err)
		}
		after, found, err := h.store.Get(ctx, created.ID)
		if err != nil || !found || after.Attributes["role"] != "reader" {
			t.Fatalf("empty patch changed state: %#v, %v, %v", after.Attributes, found, err)
		}

		revoked, err := m.Create(ctx, "bob", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Revoke(ctx, revoked.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := m.PatchAttributes(ctx, revoked.ID, nil, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PatchAttributes(revoked) = %v, want ErrNotFound", err)
		}

		expired, err := m.Create(ctx, "carol", nil)
		if err != nil {
			t.Fatal(err)
		}
		h.advance(25 * time.Hour)
		if _, err := m.PatchAttributes(ctx, expired.ID, nil, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PatchAttributes(expired) = %v, want ErrNotFound", err)
		}

		m.stop()
		if _, err := m.PatchAttributes(ctx, created.ID, nil, nil); !errors.Is(err, ErrStopped) {
			t.Fatalf("PatchAttributes after stop = %v, want ErrStopped", err)
		}
	})
}

// A custom Store without the PatchStore capability is refused instead of being
// guessed at, and a nil context is rejected before the capability check.
func TestManagerPatchAttributesReportsUnsupportedStores(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := testManager(t, &recordingStore{}, now)
	if _, err := m.PatchAttributes(context.Background(), testID(98, 32), nil, nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("PatchAttributes on a plain Store = %v, want ErrNotSupported", err)
	}
	if _, err := m.PatchAttributes(nil, testID(98, 32), nil, nil); err == nil || errors.Is(err, ErrNotSupported) {
		t.Fatalf("PatchAttributes(nil context) = %v, want a context error before the capability check", err)
	}
}

// The patch must not move either deadline: the absolute expiry stays, the idle
// lease keeps its original deadline and last-touch time, and the session dies
// exactly when the untouched idle lease ends.
func TestMemoryStorePatchAttributesLeavesDeadlinesUntouched(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	ctx := context.Background()
	id := testID(99, 32)
	if err := store.Create(ctx, sessionValue(id, "alice", now, 2*time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	key := IDDigest(id)
	before := store.records[key]

	now = now.Add(10 * time.Minute)
	if _, ok, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, []string{"nested"}); err != nil || !ok {
		t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
	}
	after := store.records[key]
	if !after.idleExpiresAt.Equal(before.idleExpiresAt) || !after.lastTouched.Equal(before.lastTouched) {
		t.Fatalf("idle lease moved: %v/%v -> %v/%v", before.idleExpiresAt, before.lastTouched, after.idleExpiresAt, after.lastTouched)
	}
	if !after.value.ExpiresAt.Equal(before.value.ExpiresAt) {
		t.Fatalf("absolute expiry moved: %v -> %v", before.value.ExpiresAt, after.value.ExpiresAt)
	}

	now = before.idleExpiresAt
	if _, found, err := store.Get(ctx, id); err != nil || found {
		t.Fatalf("Get() at the original idle deadline found=%v err=%v; the patch must not have renewed the lease", found, err)
	}
}

// A closed MemoryStore refuses the patch instead of writing to erased state.
func TestMemoryStorePatchAttributesRejectsUseAfterClose(t *testing.T) {
	store, err := NewMemoryStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PatchAttributes(context.Background(), testID(100, 32), nil, nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("PatchAttributes on a closed store = %v, want ErrStopped", err)
	}
}

// The Redis patch is one single-key script that keeps the key TTL, the absolute
// deadline, the last-touch field, and the subject index exactly as they were,
// and it refuses a key whose absolute lifetime has passed.
func TestRedisStorePatchAttributesKeepsTTLAndIndexMembership(t *testing.T) {
	if !strings.Contains(redisPatchAttributesSource, "KEYS[1]") {
		t.Fatal("patch attributes script does not address KEYS[1]")
	}
	if strings.Contains(redisPatchAttributesSource, "KEYS[2]") {
		t.Fatal("patch attributes script addresses a second key; the patch must stay single-key")
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	id := testID(101, 32)
	digest := IDDigest(id)
	if err := store.Create(ctx, sessionValue(id, "alice", now, 2*time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	key, setKey := store.key(id), store.subjectKey("alice")
	ttlBefore := server.TTL(key)
	touchedBefore := server.HGet(key, "touched")
	absoluteBefore := server.HGet(key, "absolute")
	membersBefore, err := server.SMembers(setKey)
	if err != nil {
		t.Fatal(err)
	}

	server.FastForward(5 * time.Minute)
	now = now.Add(5 * time.Minute)
	if _, ok, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, []string{"nested"}); err != nil || !ok {
		t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
	}
	if ttl := server.TTL(key); ttl != ttlBefore-5*time.Minute {
		t.Fatalf("TTL = %v, want the untouched remainder %v", ttl, ttlBefore-5*time.Minute)
	}
	if touched := server.HGet(key, "touched"); touched != touchedBefore {
		t.Fatalf("touched = %q, want the untouched %q", touched, touchedBefore)
	}
	if absolute := server.HGet(key, "absolute"); absolute != absoluteBefore {
		t.Fatalf("absolute = %q, want the untouched %q", absolute, absoluteBefore)
	}
	membersAfter, err := server.SMembers(setKey)
	if err != nil || !slicesEqual(membersAfter, membersBefore) || !slicesEqual(membersAfter, []string{digest}) {
		t.Fatalf("index = %v, %v; want the unchanged member %s", membersAfter, err, digest)
	}
	payload := server.HGet(key, "value")
	if strings.Contains(payload, `"role":"admin"`) || !strings.Contains(payload, `"role":"reader"`) {
		t.Fatalf("stored payload = %s, want only the patched attributes", payload)
	}

	shortID := testID(102, 32)
	if err := store.Create(ctx, sessionValue(shortID, "bob", now, time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Minute)
	now = now.Add(2 * time.Minute)
	if _, ok, err := store.PatchAttributes(ctx, shortID, map[string]any{"role": "reader"}, nil); err != nil || ok {
		t.Fatalf("PatchAttributes(expired) ok=%v err=%v; want not found", ok, err)
	}
	if server.Exists(store.key(shortID)) {
		t.Fatal("the expired session survived the refused patch")
	}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// The application-side merge preserves JSON values that a cjson round trip
// would damage: an empty object stays an object, an empty array stays an array,
// a wide integer keeps its digits instead of becoming scientific notation, and
// unicode and escape sequences survive. This is the losslessness evidence for
// the compare-and-swap choice documented on the Redis patch script.
func TestPatchAttributesPreservesJSONEdgeValues(t *testing.T) {
	edge := map[string]any{
		"empty_object": map[string]any{},
		"empty_array":  []any{},
		"big_integer":  int64(1234567890123456),
		"unicode":      "héllo 世界",
		"escapes":      "quote\" backslash\\ newline\n control\u0001",
		"nested_null":  map[string]any{"gone": nil},
		"disabled":     false,
	}
	normalizedEdge, err := normalizeAttributes(edge)
	if err != nil {
		t.Fatal(err)
	}
	want, err := normalizeAttributes(patchedAttributes(normalizedEdge, map[string]any{"trigger": 1}, nil))
	if err != nil {
		t.Fatal(err)
	}

	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		id := testID(103, 32)
		value := Session{
			ID:         id,
			Subject:    "alice",
			Attributes: edge,
			IssuedAt:   *h.now,
			ExpiresAt:  h.now.Add(time.Hour),
		}
		if err := h.store.Create(ctx, value, 30*time.Minute); err != nil {
			t.Fatal(err)
		}
		patched, ok, err := h.patchStore(t).PatchAttributes(ctx, id, map[string]any{"trigger": 1}, nil)
		if err != nil || !ok {
			t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
		}
		if !reflect.DeepEqual(patched.Attributes, want) {
			t.Fatalf("patched attributes = %#v, want %#v", patched.Attributes, want)
		}
		stored, found, err := h.store.Get(ctx, id)
		if err != nil || !found {
			t.Fatalf("Get() found=%v err=%v", found, err)
		}
		if !reflect.DeepEqual(stored.Attributes, want) {
			t.Fatalf("stored attributes = %#v, want %#v", stored.Attributes, want)
		}
	})
}

// The raw Redis payload proves the edge values were written without a lossy
// Lua encode: the empty containers are their JSON forms, the wide integer is
// plain digits (no exponent), and unicode and the control-character escape are
// byte-for-byte what encoding/json produced.
func TestRedisStorePatchAttributesWritesLosslessJSON(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	id := testID(104, 32)
	value := Session{
		ID:      id,
		Subject: "alice",
		Attributes: map[string]any{
			"empty_object": map[string]any{},
			"empty_array":  []any{},
			"big_integer":  int64(1234567890123456),
			"unicode":      "héllo 世界",
			"escapes":      "control\u0001",
		},
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.Create(ctx, value, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.PatchAttributes(ctx, id, map[string]any{"trigger": true}, nil); err != nil || !ok {
		t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
	}
	payload := server.HGet(store.key(id), "value")
	for _, want := range []string{
		`"empty_object":{}`,
		`"empty_array":[]`,
		`"big_integer":1234567890123456`,
		`"unicode":"héllo 世界"`,
		`control\u0001`,
		`"trigger":true`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("stored payload = %s, want it to contain %s", payload, want)
		}
	}
	if strings.Contains(payload, "e+") {
		t.Fatalf("stored payload used scientific notation: %s", payload)
	}
}

// A concurrent writer between the read and the compare-and-swap makes the first
// attempt lose; the retry merges the fresh payload, so both writers' changes
// survive.
func TestRedisStorePatchAttributesRetriesAConcurrentWriter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	id := testID(105, 32)
	if err := store.Create(ctx, sessionValue(id, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	var armed atomic.Bool
	armed.Store(true)
	var writerErr error
	client.AddHook(&commandHook{after: func(cmd goredis.Cmder) {
		if cmd.Name() != "hget" || !armed.CompareAndSwap(true, false) {
			return
		}
		if _, _, err := store.UpdateAttributes(ctx, id, map[string]any{"other": "kept"}); err != nil {
			writerErr = err
		}
	}})

	patched, ok, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, []string{"nested"})
	if err != nil || !ok {
		t.Fatalf("PatchAttributes() ok=%v err=%v (writer error %v)", ok, err, writerErr)
	}
	if writerErr != nil {
		t.Fatalf("concurrent writer error = %v", writerErr)
	}
	if patched.Attributes["role"] != "reader" || patched.Attributes["other"] != "kept" {
		t.Fatalf("attributes = %#v, want both writers' changes", patched.Attributes)
	}
	if _, present := patched.Attributes["nested"]; present {
		t.Fatalf("unset key survived: %#v", patched.Attributes)
	}
}

// A writer that keeps changing the value makes every compare-and-swap fail. The
// patch exhausts its bounded retries and reports ErrConflict without changing
// the session or its deadlines.
func TestRedisStorePatchAttributesReportsConflictWhenRetriesRunOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	server, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	id := testID(106, 32)
	if err := store.Create(ctx, sessionValue(id, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	key := store.key(id)
	ttlBefore := server.TTL(key)

	var inHook atomic.Bool
	var noise atomic.Int64
	client.AddHook(&commandHook{after: func(cmd goredis.Cmder) {
		if cmd.Name() != "hget" || inHook.Load() {
			return
		}
		inHook.Store(true)
		defer inHook.Store(false)
		_, _, _ = store.UpdateAttributes(ctx, id, map[string]any{"noise": strconv.FormatInt(noise.Add(1), 10)})
	}})

	value, ok, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, nil)
	if !errors.Is(err, ErrConflict) || ok {
		t.Fatalf("PatchAttributes() = %#v, %v, %v; want ErrConflict", value, ok, err)
	}
	if _, found, err := store.Get(ctx, id); err != nil || !found {
		t.Fatalf("the session was lost to a conflict: found=%v err=%v", found, err)
	}
	if ttl := server.TTL(key); ttl != ttlBefore {
		t.Fatalf("a conflict moved the TTL: %v -> %v", ttlBefore, ttl)
	}
	if strings.Contains(server.HGet(key, "value"), `"role":"reader"`) {
		t.Fatal("a conflicting patch wrote its attributes anyway")
	}
}

// The core value of a patch over a whole-map update: concurrent patches of
// different keys all survive on both backends.
func TestConcurrentPatchAttributesOfDifferentKeysAllSurvive(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)
		created, err := m.Create(ctx, "alice", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		store := h.patchStore(t)

		const writers = 8
		expected := make(map[string]any, writers)
		var wg sync.WaitGroup
		failures := make(chan error, writers)
		for i := range writers {
			key := fmt.Sprintf("key-%d", i)
			value := i
			expected[key] = float64(value)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := store.PatchAttributes(ctx, created.ID, map[string]any{key: value}, nil); err != nil {
					failures <- err
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Fatalf("concurrent PatchAttributes() error = %v", err)
		}

		stored, found, err := h.store.Get(ctx, created.ID)
		if err != nil || !found {
			t.Fatalf("Get() found=%v err=%v", found, err)
		}
		if !reflect.DeepEqual(stored.Attributes, expected) {
			t.Fatalf("attributes = %#v, want every writer's key %#v", stored.Attributes, expected)
		}
	})
}

// A patch sends one read and one single-key script; nothing about it can make a
// cluster deployment refuse the command.
func TestRedisStorePatchAttributesStaysSingleKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, client, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	warmID := testID(107, 32)
	if err := store.Create(ctx, sessionValue(warmID, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	// Warm the script cache so a missing EVALSHA does not add a command.
	if _, ok, err := store.PatchAttributes(ctx, warmID, map[string]any{"role": "reader"}, nil); err != nil || !ok {
		t.Fatalf("warm PatchAttributes() ok=%v err=%v", ok, err)
	}

	recorder := &commandRecorder{}
	client.AddHook(recorder)
	id := testID(108, 32)
	if err := store.Create(ctx, sessionValue(id, "bob", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	recorder.start()
	if _, ok, err := store.PatchAttributes(ctx, id, map[string]any{"role": "reader"}, nil); err != nil || !ok {
		t.Fatalf("PatchAttributes() ok=%v err=%v", ok, err)
	}
	records := recorder.stop()

	wantNames := []string{"hget", "evalsha"}
	if len(records) != len(wantNames) {
		t.Fatalf("patch sent %v, want %v", recordedNames(records), wantNames)
	}
	for i, want := range wantNames {
		if records[i].name != want {
			t.Fatalf("patch sent %v, want %v", recordedNames(records), wantNames)
		}
		keys := recordedKeys(t, records[i])
		if len(keys) != 1 || keys[0] != store.key(id) {
			t.Fatalf("patch step %d (%s) addressed %v, want only %s", i+1, records[i].name, keys, store.key(id))
		}
	}
}
