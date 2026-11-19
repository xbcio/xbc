package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// subsetOf reports whether every value in members appears in allowed.
func subsetOf(members, allowed []string) bool {
	for _, member := range members {
		if !slices.Contains(allowed, member) {
			return false
		}
	}
	return true
}

// attributeStore exposes the AttributeStore capability of a bare backend
// harness.
func (h *indexHarness) attributeStore(t *testing.T) AttributeStore {
	t.Helper()
	store, ok := h.store.(AttributeStore)
	if !ok {
		t.Fatalf("backend store %T does not implement AttributeStore", h.store)
	}
	return store
}

// Conformance 13: UpdateAttributes replaces the attributes wholesale on both
// built-in backends while leaving identity, times, and the subject index alone.
func TestAttributeStoresReplaceAttributesInPlace(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		id, digest := h.create(t, 70, "alice", time.Hour)
		before, found, err := h.store.Get(ctx, id)
		if err != nil || !found {
			t.Fatalf("Get() found=%v err=%v", found, err)
		}

		input := map[string]any{"role": "reader", "nested": map[string]any{"team": "ops"}}
		updated, ok, err := h.attributeStore(t).UpdateAttributes(ctx, id, input)
		if err != nil || !ok {
			t.Fatalf("UpdateAttributes() ok=%v err=%v", ok, err)
		}
		if updated.ID != id || updated.Subject != before.Subject || !updated.IssuedAt.Equal(before.IssuedAt) || !updated.ExpiresAt.Equal(before.ExpiresAt) {
			t.Fatalf("updated identity = %#v, want ID/subject/times from %#v", updated, before)
		}
		if updated.Attributes["role"] != "reader" || updated.Attributes["nested"].(map[string]any)["team"] != "ops" {
			t.Fatalf("updated attributes = %#v", updated.Attributes)
		}
		if len(updated.Attributes) != 2 {
			t.Fatalf("attributes = %#v, want exactly the replacement map", updated.Attributes)
		}

		// Both directions are defensive: neither the input map nor a result map
		// may reach the stored value.
		input["role"] = "mutated"
		updated.Attributes["role"] = "mutated"
		stored, found, err := h.store.Get(ctx, id)
		if err != nil || !found {
			t.Fatalf("Get() after update found=%v err=%v", found, err)
		}
		if stored.Attributes["role"] != "reader" {
			t.Fatalf("stored attributes were aliased: %#v", stored.Attributes)
		}

		infos, err := h.store.ListBySubject(ctx, "alice")
		if err != nil || len(infos) != 1 || infos[0].Digest != digest {
			t.Fatalf("List() = %v, %v; want the same indexed session", infos, err)
		}
		if infos[0].Attributes["role"] != "reader" {
			t.Fatalf("List() attributes = %#v, want the updated value", infos[0].Attributes)
		}
	})
}

// Conformance 14: UpdateAttributes refuses every dead session identically and
// never creates a record or an index member.
func TestAttributeStoresRefuseExpiredRevokedAndUnknownSessions(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		store := h.attributeStore(t)

		expiredID, _ := h.create(t, 71, "alice", time.Minute)
		revokedID, _ := h.create(t, 72, "alice", time.Hour)
		if err := h.store.Delete(ctx, revokedID); err != nil {
			t.Fatal(err)
		}
		h.advance(2 * time.Minute)
		membersBefore := h.members("alice")

		for name, id := range map[string]string{
			"expired":       expiredID,
			"revoked":       revokedID,
			"never created": testID(73, 32),
		} {
			value, found, err := store.UpdateAttributes(ctx, id, map[string]any{"role": "reader"})
			if err != nil || found {
				t.Fatalf("%s UpdateAttributes() = %#v, %v, %v; want not found", name, value, found, err)
			}
		}
		if members := h.members("alice"); !subsetOf(members, membersBefore) {
			t.Fatalf("index = %v, want no member beyond the pre-update %v; a refused update must not add one", members, membersBefore)
		}
		if slices.Contains(h.members("alice"), IDDigest(testID(73, 32))) {
			t.Fatal("a refused update indexed the never-created session")
		}
		if h.liveTotal() != 0 {
			t.Fatalf("%d sessions exist; a refused update must not create one", h.liveTotal())
		}
	})
}

// Conformance 15: empty IDs, non-JSON attributes, and a nil context are
// parameter errors rather than lookups.
func TestAttributeStoresRejectEmptyIDsAndInvalidAttributes(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		if _, _, err := h.attributeStore(t).UpdateAttributes(ctx, "", nil); err == nil {
			t.Fatal("UpdateAttributes accepted an empty ID")
		}
		if _, _, err := h.attributeStore(t).UpdateAttributes(ctx, testID(74, 32), map[string]any{"bad": make(chan int)}); err == nil {
			t.Fatal("UpdateAttributes accepted a non-JSON attribute")
		}
		if _, _, err := h.attributeStore(t).UpdateAttributes(nil, testID(74, 32), nil); err == nil {
			t.Fatal("UpdateAttributes accepted a nil context")
		}
	})
}

// The manager keeps the ID, subject, and both timestamps exactly, replaces the
// attributes, and surfaces ErrNotFound for every dead session.
func TestManagerUpdateAttributesPreservesLifetimeAndRefusesDeadSessions(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)
		created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.UpdateAttributes(ctx, created.ID, map[string]any{"bad": make(chan int)}); err == nil {
			t.Fatal("UpdateAttributes accepted a non-JSON attribute")
		}
		if _, err := m.UpdateAttributes(ctx, "attacker-selected", nil); !errors.Is(err, ErrInvalidCookie) {
			t.Fatalf("UpdateAttributes(malformed ID) = %v, want ErrInvalidCookie", err)
		}
		if _, err := m.UpdateAttributes(ctx, testID(75, 32), nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateAttributes(unknown ID) = %v, want ErrNotFound", err)
		}

		updated, err := m.UpdateAttributes(ctx, created.ID, map[string]any{"role": "reader"})
		if err != nil {
			t.Fatal(err)
		}
		if updated.ID != created.ID || updated.Subject != created.Subject ||
			!updated.IssuedAt.Equal(created.IssuedAt) || !updated.ExpiresAt.Equal(created.ExpiresAt) {
			t.Fatalf("updated = %#v, want identity and lifetime of %#v", updated, created)
		}
		if updated.Attributes["role"] != "reader" {
			t.Fatalf("updated attributes = %#v", updated.Attributes)
		}
		value, verdict, err := m.Verify(ctx, created.ID)
		if err != nil || verdict != VerdictValid || value.Attributes["role"] != "reader" {
			t.Fatalf("Verify() = %#v, %v, %v; want the updated session", value, verdict, err)
		}

		if _, err := m.UpdateAttributes(ctx, created.ID, nil); err != nil {
			t.Fatalf("UpdateAttributes(nil attributes) = %v", err)
		}
		after, found, err := h.store.Get(ctx, created.ID)
		if err != nil || !found || after.Attributes != nil {
			t.Fatalf("nil attributes were not stored as nil: %#v, %v, %v", after.Attributes, found, err)
		}

		revoked, err := m.Create(ctx, "bob", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Revoke(ctx, revoked.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := m.UpdateAttributes(ctx, revoked.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateAttributes(revoked) = %v, want ErrNotFound", err)
		}

		expired, err := m.Create(ctx, "carol", nil)
		if err != nil {
			t.Fatal(err)
		}
		h.advance(25 * time.Hour)
		if _, err := m.UpdateAttributes(ctx, expired.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateAttributes(expired) = %v, want ErrNotFound", err)
		}

		m.stop()
		if _, err := m.UpdateAttributes(ctx, created.ID, nil); !errors.Is(err, ErrStopped) {
			t.Fatalf("UpdateAttributes after stop = %v, want ErrStopped", err)
		}
	})
}

// A custom Store without the AttributeStore capability is refused instead of
// being guessed at.
func TestManagerUpdateAttributesReportsUnsupportedStores(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := testManager(t, &recordingStore{}, now)
	if _, err := m.UpdateAttributes(context.Background(), testID(76, 32), nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("UpdateAttributes on a plain Store = %v, want ErrNotSupported", err)
	}
	if _, err := m.UpdateAttributes(nil, testID(76, 32), nil); err == nil || errors.Is(err, ErrNotSupported) {
		t.Fatalf("UpdateAttributes(nil context) = %v, want a context error before the capability check", err)
	}
}

// The attributes update must not move either deadline: the absolute expiry
// stays, the idle lease keeps its original deadline and last-touch time, and
// the session dies exactly when the untouched idle lease ends.
func TestMemoryStoreUpdateAttributesLeavesDeadlinesUntouched(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	ctx := context.Background()
	id := testID(77, 32)
	if err := store.Create(ctx, sessionValue(id, "alice", now, 2*time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	key := IDDigest(id)
	before := store.records[key]

	now = now.Add(10 * time.Minute)
	if _, ok, err := store.UpdateAttributes(ctx, id, map[string]any{"role": "reader"}); err != nil || !ok {
		t.Fatalf("UpdateAttributes() ok=%v err=%v", ok, err)
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
		t.Fatalf("Get() at the original idle deadline found=%v err=%v; the update must not have renewed the lease", found, err)
	}
}

// The Redis update is one single-key script that keeps the key TTL and the
// subject index exactly as they were, and it refuses a key whose absolute
// lifetime has passed.
func TestRedisStoreUpdateAttributesKeepsTTLAndIndexMembership(t *testing.T) {
	if !strings.Contains(redisUpdateAttributesSource, "KEYS[1]") {
		t.Fatal("update attributes script does not address KEYS[1]")
	}
	if strings.Contains(redisUpdateAttributesSource, "KEYS[2]") {
		t.Fatal("update attributes script addresses a second key; the update must stay single-key")
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	ctx := context.Background()
	id := testID(78, 32)
	digest := IDDigest(id)
	if err := store.Create(ctx, sessionValue(id, "alice", now, 2*time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	key, setKey := store.key(id), store.subjectKey("alice")
	ttlBefore := server.TTL(key)
	membersBefore, err := server.SMembers(setKey)
	if err != nil {
		t.Fatal(err)
	}

	server.FastForward(5 * time.Minute)
	now = now.Add(5 * time.Minute)
	if _, ok, err := store.UpdateAttributes(ctx, id, map[string]any{"role": "reader"}); err != nil || !ok {
		t.Fatalf("UpdateAttributes() ok=%v err=%v", ok, err)
	}
	if ttl := server.TTL(key); ttl != ttlBefore-5*time.Minute {
		t.Fatalf("TTL = %v, want the untouched remainder %v", ttl, ttlBefore-5*time.Minute)
	}
	membersAfter, err := server.SMembers(setKey)
	if err != nil || !slices.Equal(membersAfter, membersBefore) || !slices.Equal(membersAfter, []string{digest}) {
		t.Fatalf("index = %v, %v; want the unchanged member %s", membersAfter, err, digest)
	}
	payload := server.HGet(key, "value")
	if strings.Contains(payload, `"role":"admin"`) || !strings.Contains(payload, `"role":"reader"`) {
		t.Fatalf("stored payload = %s, want only the replaced attributes", payload)
	}

	shortID := testID(79, 32)
	if err := store.Create(ctx, sessionValue(shortID, "bob", now, time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Minute)
	now = now.Add(2 * time.Minute)
	if _, ok, err := store.UpdateAttributes(ctx, shortID, map[string]any{"role": "reader"}); err != nil || ok {
		t.Fatalf("UpdateAttributes(expired) ok=%v err=%v; want not found", ok, err)
	}
}

// A closed MemoryStore refuses the update instead of writing to erased state.
func TestMemoryStoreUpdateAttributesRejectsUseAfterClose(t *testing.T) {
	store, err := NewMemoryStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateAttributes(context.Background(), testID(85, 32), nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("UpdateAttributes on a closed store = %v, want ErrStopped", err)
	}
}

// Updates racing a revocation or a rotation must never resurrect the old ID,
// must not leave a stray record behind, and must keep the index consistent.
func TestUpdateAttributesRacesWithRevokeAndRotateWithoutResurrecting(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()

		t.Run("revoke", func(t *testing.T) {
			m := h.manager(t)
			created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
			if err != nil {
				t.Fatal(err)
			}
			failures := make(chan error, 32)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for round := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := m.UpdateAttributes(ctx, created.ID, map[string]any{"round": round}); err != nil && !errors.Is(err, ErrNotFound) {
						failures <- err
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := m.Revoke(ctx, created.ID); err != nil {
					failures <- err
				}
			}()
			close(start)
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			h.assertAlive(t, created.ID, false)
			if members := h.members("alice"); len(members) != 0 {
				t.Fatalf("index after the race = %v, want empty", members)
			}
			if h.liveTotal() != 0 {
				t.Fatalf("%d records survived a revocation race", h.liveTotal())
			}
		})

		t.Run("rotate", func(t *testing.T) {
			m := h.manager(t)
			created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
			if err != nil {
				t.Fatal(err)
			}
			failures := make(chan error, 32)
			var wg sync.WaitGroup
			start := make(chan struct{})
			var rotated Session
			for round := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := m.UpdateAttributes(ctx, created.ID, map[string]any{"round": round}); err != nil && !errors.Is(err, ErrNotFound) {
						failures <- err
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				value, err := m.Rotate(ctx, created.ID)
				if err != nil && !errors.Is(err, ErrNotFound) {
					failures <- err
				}
				rotated = value
			}()
			close(start)
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			h.assertAlive(t, created.ID, false)
			if rotated.ID == "" {
				return
			}
			if _, found, err := h.store.Get(ctx, rotated.ID); err != nil || !found {
				t.Fatalf("replacement lookup found=%v err=%v", found, err)
			}
			if _, err := m.UpdateAttributes(ctx, created.ID, nil); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateAttributes(old ID) after rotation = %v, want ErrNotFound", err)
			}
			if _, _, err := h.attributeStore(t).UpdateAttributes(ctx, created.ID, nil); err != nil {
				t.Fatalf("store UpdateAttributes on the old ID = %v; a missing session is not an error", err)
			}
			if members := h.members("alice"); !slices.Equal(members, []string{IDDigest(rotated.ID)}) {
				t.Fatalf("index = %v, want only the replacement", members)
			}
		})
	})
}
