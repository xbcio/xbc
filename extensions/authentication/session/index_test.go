package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// indexHarness exposes one IndexedStore backend to the conformance cases below
// together with the white-box handles they need: a shared clock, the raw index
// members of a subject, a way to plant a member the store never wrote, and a
// liveness probe that does not go through the store under test.
type indexHarness struct {
	store   IndexedStore
	now     *time.Time
	advance func(time.Duration)
	members func(subject string) []string
	inject  func(subject, digest string)
	live    func(digest string) bool
	// liveTotal counts every stored session regardless of subject.
	liveTotal func() int
	// exact is true when the backend writes a session and its index entry under
	// one lock, so "no live session is ever unindexed" holds under concurrency.
	exact bool
}

type indexBackend struct {
	name string
	make func(*testing.T) *indexHarness
}

var indexBackends = []indexBackend{
	{"memory", newMemoryIndexHarness},
	{"redis", newRedisIndexHarness},
}

func forEachIndexedStore(t *testing.T, run func(*testing.T, *indexHarness)) {
	t.Helper()
	for _, backend := range indexBackends {
		t.Run(backend.name, func(t *testing.T) { run(t, backend.make(t)) })
	}
}

func newMemoryIndexHarness(t *testing.T) *indexHarness {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	return &indexHarness{
		store:   store,
		now:     &now,
		exact:   true,
		advance: func(d time.Duration) { now = now.Add(d) },
		members: func(subject string) []string {
			store.mu.Lock()
			defer store.mu.Unlock()
			out := make([]string, 0, len(store.index[subject]))
			for digest := range store.index[subject] {
				out = append(out, digest)
			}
			slices.Sort(out)
			return out
		},
		inject: func(subject, digest string) {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.indexLocked(subject, digest)
		},
		live: func(digest string) bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			record, ok := store.records[digest]
			return ok && record.live(now)
		},
		liveTotal: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return len(store.records)
		},
	}
}

func newRedisIndexHarness(t *testing.T) *indexHarness {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	server, _, store := newRedisStoreTest(t, &now)
	return &indexHarness{
		store: store,
		now:   &now,
		advance: func(d time.Duration) {
			server.FastForward(d)
			now = now.Add(d)
		},
		members: func(subject string) []string {
			members, err := server.SMembers(store.subjectKey(subject))
			if err != nil {
				return nil
			}
			slices.Sort(members)
			return members
		},
		inject: func(subject, digest string) {
			if _, err := server.SAdd(store.subjectKey(subject), digest); err != nil {
				t.Fatal(err)
			}
		},
		live: func(digest string) bool { return server.Exists(store.prefix + digest) },
		liveTotal: func() int {
			total := 0
			for _, key := range server.Keys() {
				if !strings.HasPrefix(key, store.prefix+"subject:") {
					total++
				}
			}
			return total
		},
	}
}

// manager builds a Manager over the harness store whose clock is the harness
// clock, so expiry the test advances is the expiry the manager sees.
func (h *indexHarness) manager(t *testing.T) *manager {
	t.Helper()
	cfg, err := normalizeConfig(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &manager{store: h.store, cfg: cfg, now: func() time.Time { return *h.now }}
}

// create stores one session for subject through the store and returns its ID
// and digest. The seed picks the ID.
func (h *indexHarness) create(t *testing.T, seed byte, subject string, lifetime time.Duration) (string, string) {
	t.Helper()
	id := testID(seed, 32)
	if err := h.store.Create(context.Background(), sessionValue(id, subject, *h.now, lifetime), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	return id, IDDigest(id)
}

func (h *indexHarness) assertAlive(t *testing.T, id string, want bool) {
	t.Helper()
	_, found, err := h.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if found != want {
		t.Fatalf("session %s alive=%v, want %v", IDDigest(id)[:8], found, want)
	}
}

func infoDigests(infos []SessionInfo) []string {
	digests := make([]string, len(infos))
	for i, info := range infos {
		digests[i] = info.Digest
	}
	slices.Sort(digests)
	return digests
}

func sortedDigests(digests ...string) []string {
	out := slices.Clone(digests)
	slices.Sort(out)
	return out
}

// Conformance 1: List reports digest, subject, attributes and times, and a
// subject sees only its own sessions.
func TestIndexedStoreListReportsOwnSessionsWithTheirFields(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		_, a1 := h.create(t, 1, "alice", time.Hour)
		h.advance(time.Minute)
		_, a2 := h.create(t, 2, "alice", 2*time.Hour)
		_, b1 := h.create(t, 3, "bob", time.Hour)

		infos, err := h.store.ListBySubject(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := infoDigests(infos), sortedDigests(a1, a2); !slices.Equal(got, want) {
			t.Fatalf("List(alice) = %v, want %v", got, want)
		}
		created := time.Unix(1_700_000_000, 0).UTC()
		for _, info := range infos {
			wantIssued, wantLifetime := created, time.Hour
			if info.Digest == a2 {
				wantIssued, wantLifetime = created.Add(time.Minute), 2*time.Hour
			}
			if info.Subject != "alice" || !info.IssuedAt.Equal(wantIssued) || !info.ExpiresAt.Equal(wantIssued.Add(wantLifetime)) || !info.LastSeenAt.Equal(wantIssued) {
				t.Fatalf("info = %#v, want issued %v lifetime %v", info, wantIssued, wantLifetime)
			}
			if info.Attributes["role"] != "admin" || info.Attributes["nested"].(map[string]any)["team"] != "core" {
				t.Fatalf("attributes = %#v", info.Attributes)
			}
		}
		other, err := h.store.ListBySubject(context.Background(), "bob")
		if err != nil || !slices.Equal(infoDigests(other), []string{b1}) {
			t.Fatalf("List(bob) = %v, %v; want only %s", infoDigests(other), err, b1[:8])
		}
		if none, err := h.store.ListBySubject(context.Background(), "carol"); err != nil || len(none) != 0 {
			t.Fatalf("List(carol) = %v, %v; want empty", none, err)
		}
		// LastSeenAt follows the idle renewal: Touch past the throttle interval
		// moves it, and a session nobody touched keeps its creation time.
		h.advance(6 * time.Minute)
		id1 := testID(1, 32)
		if _, found, err := h.store.Touch(context.Background(), id1, 30*time.Minute, 5*time.Minute); err != nil || !found {
			t.Fatalf("Touch found=%v err=%v", found, err)
		}
		renewed, err := h.store.ListBySubject(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range renewed {
			want := created.Add(time.Minute)
			if info.Digest == a1 {
				want = *h.now
			}
			if !info.LastSeenAt.Equal(want) {
				t.Fatalf("LastSeenAt of %s = %v, want %v", info.Digest[:8], info.LastSeenAt, want)
			}
		}
	})
}

// Conformance 2: attributes are copied in both directions.
func TestIndexedStoreListAttributesAreDefensiveCopies(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		input := sessionValue(testID(4, 32), "alice", *h.now, time.Hour)
		if err := h.store.Create(context.Background(), input, 30*time.Minute); err != nil {
			t.Fatal(err)
		}
		// A caller mutating its input after Create must not reach the store.
		input.Attributes["role"] = "mutated"
		input.Attributes["nested"].(map[string]any)["team"] = "mutated"
		first, err := h.store.ListBySubject(context.Background(), "alice")
		if err != nil || len(first) != 1 {
			t.Fatalf("List = %v, %v", first, err)
		}
		if first[0].Attributes["role"] != "admin" || first[0].Attributes["nested"].(map[string]any)["team"] != "core" {
			t.Fatalf("Create input aliased into the store: %#v", first[0].Attributes)
		}
		// A caller mutating a result must not reach the store either.
		first[0].Attributes["role"] = "reader"
		first[0].Attributes["nested"].(map[string]any)["team"] = "other"
		second, err := h.store.ListBySubject(context.Background(), "alice")
		if err != nil || len(second) != 1 {
			t.Fatalf("List = %v, %v", second, err)
		}
		if second[0].Attributes["role"] != "admin" || second[0].Attributes["nested"].(map[string]any)["team"] != "core" {
			t.Fatalf("List result aliased the stored attributes: %#v", second[0].Attributes)
		}
	})
}

// Conformance 3: RevokeBySubject ends every session of the subject, only that
// subject's, counts them, and is idempotent.
func TestIndexedStoreRevokeBySubjectEndsOnlyThatSubjectsSessions(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		var aliceIDs []string
		for seed := byte(10); seed < 13; seed++ {
			id, _ := h.create(t, seed, "alice", time.Hour)
			aliceIDs = append(aliceIDs, id)
		}
		bobID, bobDigest := h.create(t, 20, "bob", time.Hour)

		revoked, err := h.store.RevokeBySubject(context.Background(), "alice")
		if err != nil || revoked != 3 {
			t.Fatalf("RevokeBySubject(alice) = %d, %v; want 3", revoked, err)
		}
		for _, id := range aliceIDs {
			h.assertAlive(t, id, false)
		}
		h.assertAlive(t, bobID, true)
		if members := h.members("alice"); len(members) != 0 {
			t.Fatalf("alice index after revoke = %v, want empty", members)
		}
		if members := h.members("bob"); !slices.Equal(members, []string{bobDigest}) {
			t.Fatalf("bob index = %v, want only bob's digest", members)
		}
		if again, err := h.store.RevokeBySubject(context.Background(), "alice"); err != nil || again != 0 {
			t.Fatalf("second RevokeBySubject = %d, %v; want 0, nil", again, err)
		}
	})
}

// Conformance 4: RevokeByDigest only acts on the subject that owns the digest,
// and a malformed digest is an error, not a "not found".
func TestIndexedStoreRevokeByDigestChecksTheOwningSubject(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		aliceID, aliceDigest := h.create(t, 30, "alice", time.Hour)
		bobID, bobDigest := h.create(t, 31, "bob", time.Hour)
		ctx := context.Background()

		// Right digest, wrong subject: nothing is touched, in either direction.
		for _, c := range []struct{ subject, digest string }{{"bob", aliceDigest}, {"alice", bobDigest}} {
			if ok, err := h.store.RevokeByDigest(ctx, c.subject, c.digest); err != nil || ok {
				t.Fatalf("RevokeByDigest(%s, foreign digest) = %v, %v; want false, nil", c.subject, ok, err)
			}
		}
		h.assertAlive(t, aliceID, true)
		h.assertAlive(t, bobID, true)

		unknown := IDDigest("no such session")
		if ok, err := h.store.RevokeByDigest(ctx, "alice", unknown); err != nil || ok {
			t.Fatalf("RevokeByDigest(unknown) = %v, %v; want false, nil", ok, err)
		}
		for _, bad := range []string{"", "abc", aliceDigest[:63], aliceDigest + "0", strings.ToUpper(aliceDigest), "../" + aliceDigest[3:], strings.Repeat("g", 64)} {
			ok, err := h.store.RevokeByDigest(ctx, "alice", bad)
			if err == nil || ok || errors.Is(err, ErrNotFound) {
				t.Fatalf("RevokeByDigest(%q) = %v, %v; want a parameter error", bad, ok, err)
			}
		}
		h.assertAlive(t, aliceID, true)

		if ok, err := h.store.RevokeByDigest(ctx, "alice", aliceDigest); err != nil || !ok {
			t.Fatalf("RevokeByDigest(owner) = %v, %v; want true, nil", ok, err)
		}
		h.assertAlive(t, aliceID, false)
		h.assertAlive(t, bobID, true)
		if members := h.members("alice"); len(members) != 0 {
			t.Fatalf("alice index = %v, want empty", members)
		}
		if ok, err := h.store.RevokeByDigest(ctx, "alice", aliceDigest); err != nil || ok {
			t.Fatalf("second RevokeByDigest = %v, %v; want false, nil", ok, err)
		}
	})
}

// Conformance 5: members whose session is gone are skipped, never counted, and
// removed -- including the ones a revoke has no session to delete for.
func TestIndexedStoreStaleMembersAreSkippedCountedOutAndPruned(t *testing.T) {
	type fixture struct {
		h                       *indexHarness
		live1, live2, dying     string
		liveID1, liveID2, bobID string
		ghost                   string
	}
	setup := func(t *testing.T, h *indexHarness) fixture {
		f := fixture{h: h, ghost: IDDigest("never created")}
		f.liveID1, f.live1 = h.create(t, 40, "alice", time.Hour)
		f.liveID2, f.live2 = h.create(t, 41, "alice", time.Hour)
		_, f.dying = h.create(t, 42, "alice", time.Minute)
		f.bobID, _ = h.create(t, 43, "bob", time.Hour)
		h.inject("alice", f.ghost)
		h.advance(2 * time.Minute)
		return f
	}
	t.Run("list", func(t *testing.T) {
		forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
			f := setup(t, h)
			infos, err := h.store.ListBySubject(context.Background(), "alice")
			if err != nil || !slices.Equal(infoDigests(infos), sortedDigests(f.live1, f.live2)) {
				t.Fatalf("List = %v, %v; want only the live sessions", infoDigests(infos), err)
			}
			if members := h.members("alice"); !slices.Equal(members, sortedDigests(f.live1, f.live2)) {
				t.Fatalf("index after List = %v; stale members must be pruned", members)
			}
		})
	})
	t.Run("revoke by subject", func(t *testing.T) {
		forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
			f := setup(t, h)
			revoked, err := h.store.RevokeBySubject(context.Background(), "alice")
			if err != nil || revoked != 2 {
				t.Fatalf("RevokeBySubject = %d, %v; want 2 (stale members not counted)", revoked, err)
			}
			if members := h.members("alice"); len(members) != 0 {
				t.Fatalf("index after revoke = %v; stale members must be removed too", members)
			}
			h.assertAlive(t, f.bobID, true)
		})
	})
	t.Run("revoke by digest", func(t *testing.T) {
		forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
			f := setup(t, h)
			for _, stale := range []string{f.ghost, f.dying} {
				if ok, err := h.store.RevokeByDigest(context.Background(), "alice", stale); err != nil || ok {
					t.Fatalf("RevokeByDigest(stale) = %v, %v; want false, nil", ok, err)
				}
			}
			if members := h.members("alice"); !slices.Equal(members, sortedDigests(f.live1, f.live2)) {
				t.Fatalf("index = %v; the stale members must be cleared", members)
			}
		})
	})
	t.Run("foreign member", func(t *testing.T) {
		forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
			f := setup(t, h)
			bobDigest := IDDigest(f.bobID)
			h.inject("alice", bobDigest)
			infos, err := h.store.ListBySubject(context.Background(), "alice")
			if err != nil || !slices.Equal(infoDigests(infos), sortedDigests(f.live1, f.live2)) {
				t.Fatalf("List(alice) = %v, %v; must not report bob's session", infoDigests(infos), err)
			}
			h.inject("alice", bobDigest)
			if ok, err := h.store.RevokeByDigest(context.Background(), "alice", bobDigest); err != nil || ok {
				t.Fatalf("RevokeByDigest(alice, bob's digest) = %v, %v; want false, nil", ok, err)
			}
			h.assertAlive(t, f.bobID, true)
		})
	})
}

// Conformance 6: Rotate moves the index entry from the old digest to the new
// one, and revoking the subject kills the rotated session.
func TestManagerRotateMovesTheIndexEntryAndSubjectRevokeKillsTheRotatedSession(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		m := h.manager(t)
		ctx := context.Background()
		created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
		if err != nil {
			t.Fatal(err)
		}
		rotated, err := m.Rotate(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		// White-box, before any List can prune: the old digest must already be
		// gone from the index, not merely dead.
		if members := h.members("alice"); !slices.Equal(members, []string{IDDigest(rotated.ID)}) {
			t.Fatalf("index after Rotate = %v, want only the rotated digest", members)
		}
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil || !slices.Equal(infoDigests(infos), []string{IDDigest(rotated.ID)}) {
			t.Fatalf("List after Rotate = %v, %v", infoDigests(infos), err)
		}
		if revoked, err := m.RevokeBySubject(ctx, "alice"); err != nil || revoked != 1 {
			t.Fatalf("RevokeBySubject = %d, %v; want 1", revoked, err)
		}
		for _, id := range []string{created.ID, rotated.ID} {
			if _, err := m.Rotate(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Rotate of a revoked ID = %v, want ErrNotFound", err)
			}
		}
	})
}

// Conformance 7: the existing Revoke(id) leaves no index entry behind, and a
// sibling session stays listed.
func TestManagerRevokeByIDRemovesTheSessionFromTheIndex(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		m := h.manager(t)
		ctx := context.Background()
		first, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		second, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Revoke(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		if members := h.members("alice"); !slices.Equal(members, []string{IDDigest(second.ID)}) {
			t.Fatalf("index after Revoke = %v, want only the sibling", members)
		}
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil || !slices.Equal(infoDigests(infos), []string{IDDigest(second.ID)}) {
			t.Fatalf("List after Revoke = %v, %v", infoDigests(infos), err)
		}
		if err := m.Revoke(ctx, first.ID); err != nil {
			t.Fatalf("revoking an already revoked ID = %v, want nil", err)
		}
	})
}

// The storm's handles are package-level so a failed run cannot strand them in a
// dead test frame: workers outlive an early t.Fatal on the driving goroutine.
var (
	stormWorkers  sync.WaitGroup
	stormFailures = make(chan string, 256)
)

// Conformance 9: parallel Create, Rotate, List and RevokeBySubject are free of
// data races (under -race) and leave no listed session dead.
func TestManagerConcurrentCreateRotateRevokeStorm(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		m := h.manager(t)
		ctx := context.Background()
		report := func(format string, args ...any) {
			select {
			case stormFailures <- fmt.Sprintf(format, args...):
			default:
			}
		}
		for worker := range 8 {
			subject := []string{"alice", "bob"}[worker%2]
			stormWorkers.Add(1)
			go func() {
				defer stormWorkers.Done()
				for round := range 25 {
					created, err := m.Create(ctx, subject, nil)
					if err != nil {
						report("Create: %v", err)
						return
					}
					if _, err := m.Rotate(ctx, created.ID); err != nil && !errors.Is(err, ErrNotFound) {
						report("Rotate: %v", err)
					}
					if round%5 == 0 {
						if _, err := m.RevokeBySubject(ctx, subject); err != nil {
							report("RevokeBySubject: %v", err)
						}
					}
					if _, err := m.ListBySubject(ctx, subject); err != nil {
						report("ListBySubject: %v", err)
					}
				}
			}()
		}
		stormWorkers.Wait()
		for len(stormFailures) > 0 {
			t.Error(<-stormFailures)
		}
		for _, subject := range []string{"alice", "bob"} {
			infos, err := m.ListBySubject(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			for _, info := range infos {
				if !h.live(info.Digest) {
					t.Fatalf("List(%s) reported dead session %s", subject, info.Digest[:8])
				}
			}
		}
		for _, subject := range []string{"alice", "bob"} {
			if _, err := m.RevokeBySubject(ctx, subject); err != nil {
				t.Fatal(err)
			}
			if members := h.members(subject); len(members) != 0 {
				t.Fatalf("%s index after the final revoke = %v", subject, members)
			}
		}
		// Only a store that writes session and index under one lock promises that
		// nothing survives unindexed; a distributed one documents the window.
		if h.exact && h.liveTotal() != 0 {
			t.Fatalf("%d sessions survived the final revoke unindexed", h.liveTotal())
		}
	})
}

// Conformance 10: a custom Store that carries no index reports ErrNotSupported
// from all three methods, through the Manager and through the Plugin.
func TestManagerReportsErrNotSupportedForAStoreWithoutAnIndex(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	plain := &recordingStore{}
	if _, ok := any(plain).(IndexedStore); ok {
		t.Fatal("the fixture store must not implement IndexedStore")
	}
	managers := map[string]Manager{"manager": testManager(t, plain, now)}
	built, err := New(DefaultConfig(), WithStore(plain))
	if err != nil {
		t.Fatal(err)
	}
	managers["plugin"] = built.Manager()
	for name, m := range managers {
		ctx := context.Background()
		if _, err := m.ListBySubject(ctx, "alice"); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("%s ListBySubject = %v, want ErrNotSupported", name, err)
		}
		if _, err := m.RevokeBySubject(ctx, "alice"); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("%s RevokeBySubject = %v, want ErrNotSupported", name, err)
		}
		if _, err := m.RevokeByDigest(ctx, "alice", IDDigest("x")); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("%s RevokeByDigest = %v, want ErrNotSupported", name, err)
		}
	}
}

// Conformance 11: after Stop every method reports ErrStopped, and an empty or
// blank subject is a parameter error that is neither ErrStopped nor
// ErrNotSupported.
func TestManagerIndexMethodsRejectEmptySubjectsAndUseAfterStop(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		m := h.manager(t)
		ctx := context.Background()
		for _, subject := range []string{"", "   ", "\t"} {
			_, listErr := m.ListBySubject(ctx, subject)
			_, revokeErr := m.RevokeBySubject(ctx, subject)
			_, digestErr := m.RevokeByDigest(ctx, subject, IDDigest("x"))
			for name, err := range map[string]error{"List": listErr, "RevokeBySubject": revokeErr, "RevokeByDigest": digestErr} {
				if err == nil || errors.Is(err, ErrStopped) || errors.Is(err, ErrNotSupported) {
					t.Fatalf("%s(%q) = %v, want a parameter error", name, subject, err)
				}
			}
			// The stores apply the same rule themselves.
			if _, err := h.store.ListBySubject(ctx, subject); err == nil {
				t.Fatalf("store List(%q) accepted an empty subject", subject)
			}
			if _, err := h.store.RevokeBySubject(ctx, subject); err == nil {
				t.Fatalf("store RevokeBySubject(%q) accepted an empty subject", subject)
			}
			if _, err := h.store.RevokeByDigest(ctx, subject, IDDigest("x")); err == nil {
				t.Fatalf("store RevokeByDigest(%q) accepted an empty subject", subject)
			}
		}
		m.stop()
		if _, err := m.ListBySubject(ctx, "alice"); !errors.Is(err, ErrStopped) {
			t.Fatalf("ListBySubject after stop = %v", err)
		}
		if _, err := m.RevokeBySubject(ctx, "alice"); !errors.Is(err, ErrStopped) {
			t.Fatalf("RevokeBySubject after stop = %v", err)
		}
		if _, err := m.RevokeByDigest(ctx, "alice", IDDigest("x")); !errors.Is(err, ErrStopped) {
			t.Fatalf("RevokeByDigest after stop = %v", err)
		}
	})
}

// A colliding ID under another subject is refused and must neither take over
// nor strip the index entry of the session that already holds it.
func TestIndexedStoreCollidingIDLeavesTheOwnersIndexEntryAlone(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		id, digest := h.create(t, 50, "alice", time.Hour)
		for _, subject := range []string{"bob", "alice"} {
			err := h.store.Create(ctx, sessionValue(id, subject, *h.now, time.Hour), 30*time.Minute)
			if !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("Create(%s) onto a held ID = %v, want ErrAlreadyExists", subject, err)
			}
		}
		if members := h.members("alice"); !slices.Equal(members, []string{digest}) {
			t.Fatalf("alice index = %v, want the owner's digest kept", members)
		}
		if members := h.members("bob"); len(members) != 0 {
			t.Fatalf("bob index = %v, want empty after the refused Create", members)
		}
		rotateOnto := h.store.Rotate(ctx, testID(51, 32), sessionValue(id, "alice", *h.now, time.Hour), 30*time.Minute)
		if !errors.Is(rotateOnto, ErrNotFound) {
			t.Fatalf("Rotate from a missing ID = %v, want ErrNotFound", rotateOnto)
		}
		h.assertAlive(t, id, true)
	})
}

func TestMemoryStoreCleanupAndCloseKeepTheIndexInStep(t *testing.T) {
	h := newMemoryIndexHarness(t)
	store := h.store.(*MemoryStore)
	_, short := h.create(t, 60, "alice", time.Minute)
	_, long := h.create(t, 61, "alice", time.Hour)
	h.advance(2 * time.Minute)
	store.cleanup(*h.now)
	if members := h.members("alice"); !slices.Equal(members, []string{long}) {
		t.Fatalf("index after cleanup = %v, want only the live digest (dropped %s)", members, short[:8])
	}
	h.advance(2 * time.Hour)
	store.cleanup(*h.now)
	store.mu.Lock()
	leftover := len(store.index)
	store.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("%d subject entries survived cleanup of every record", leftover)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.ListBySubject(ctx, "alice"); !errors.Is(err, ErrStopped) {
		t.Fatalf("List on a closed store = %v", err)
	}
	if _, err := store.RevokeBySubject(ctx, "alice"); !errors.Is(err, ErrStopped) {
		t.Fatalf("RevokeBySubject on a closed store = %v", err)
	}
	if _, err := store.RevokeByDigest(ctx, "alice", short); !errors.Is(err, ErrStopped) {
		t.Fatalf("RevokeByDigest on a closed store = %v", err)
	}
}
