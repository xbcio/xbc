package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// testManagerWithConfig builds a manager over store with a mutable set of
// configuration overrides, so a test can exercise max_sessions without going
// through the plugin constructor.
func testManagerWithConfig(t *testing.T, store Store, now func() time.Time, mutate func(*Config)) *manager {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &manager{store: store, cfg: normalized, now: now}
}

func (h *indexHarness) managerWithConfig(t *testing.T, mutate func(*Config)) *manager {
	t.Helper()
	return testManagerWithConfig(t, h.store, func() time.Time { return *h.now }, mutate)
}

func TestConfigSessionLimitDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxSessions != 0 || cfg.OnExceed != OnExceedRevokeOldest {
		t.Fatalf("defaults = %d, %q; want 0, %q", cfg.MaxSessions, cfg.OnExceed, OnExceedRevokeOldest)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.maxSessions != 0 || normalized.onExceed != OnExceedRevokeOldest {
		t.Fatalf("normalized = %d, %q", normalized.maxSessions, normalized.onExceed)
	}

	cfg = DefaultConfig()
	cfg.MaxSessions = 3
	cfg.OnExceed = "  REJECT_NEW "
	normalized, err = normalizeConfig(cfg)
	if err != nil || normalized.maxSessions != 3 || normalized.onExceed != OnExceedRejectNew {
		t.Fatalf("normalized limit settings = %#v, %v", normalized, err)
	}

	cfg = DefaultConfig()
	cfg.OnExceed = ""
	normalized, err = normalizeConfig(cfg)
	if err != nil || normalized.onExceed != OnExceedRevokeOldest {
		t.Fatalf("blank on_exceed = %q, %v; want the default", normalized.onExceed, err)
	}
}

// The limit revokes the earliest-issued live session before creating the new
// one, reports the eviction as an EventRevoked with the evicted digest and
// Count 1, and leaves exactly max_sessions live sessions.
func TestManagerCreateRevokesTheOldestSessionAtTheLimit(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.managerWithConfig(t, func(c *Config) { c.MaxSessions = 2 })
		var events []Event
		m.observers = []Observer{observerFunc(func(_ context.Context, event Event) {
			events = append(events, event)
		})}

		first, err := m.Create(ctx, "alice", map[string]any{"n": 1})
		if err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
		second, err := m.Create(ctx, "alice", map[string]any{"n": 2})
		if err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
		third, err := m.Create(ctx, "alice", map[string]any{"n": 3})
		if err != nil {
			t.Fatal(err)
		}

		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := infoDigests(infos), sortedDigests(IDDigest(second.ID), IDDigest(third.ID)); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("sessions after the third Create = %v, want %v", got, want)
		}
		if _, found, err := h.store.Get(ctx, first.ID); err != nil || found {
			t.Fatalf("the earliest session survived the limit: found=%v err=%v", found, err)
		}

		wantKinds := []EventKind{EventCreated, EventCreated, EventRevoked, EventCreated}
		if len(events) != len(wantKinds) {
			t.Fatalf("events = %v, want %v", eventKinds(events), wantKinds)
		}
		for i, want := range wantKinds {
			if events[i].Kind != want {
				t.Fatalf("event %d = %v, want %v (sequence %v)", i, events[i].Kind, want, eventKinds(events))
			}
		}
		evicted := events[2]
		if evicted.Subject != "alice" || evicted.Digest != IDDigest(first.ID) || evicted.Count != 1 || evicted.Verdict != VerdictValid {
			t.Fatalf("eviction event = %#v, want alice's earliest digest with Count 1", evicted)
		}

		h.advance(time.Minute)
		fourth, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		infos, err = m.ListBySubject(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := infoDigests(infos), sortedDigests(IDDigest(third.ID), IDDigest(fourth.ID)); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("sessions after the fourth Create = %v, want %v", got, want)
		}
		if _, found, err := h.store.Get(ctx, second.ID); err != nil || found {
			t.Fatalf("the next-earliest session survived: found=%v err=%v", found, err)
		}
	})
}

// reject_new refuses the new session, leaves the existing ones untouched, and
// reports no eviction.
func TestManagerCreateRejectsNewSessionsAtTheLimit(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.managerWithConfig(t, func(c *Config) {
			c.MaxSessions = 2
			c.OnExceed = OnExceedRejectNew
		})
		var events []Event
		m.observers = []Observer{observerFunc(func(_ context.Context, event Event) {
			events = append(events, event)
		})}

		first, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		second, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := range 2 {
			if _, err := m.Create(ctx, "alice", nil); !errors.Is(err, ErrTooManySessions) {
				t.Fatalf("Create past the limit (attempt %d) = %v, want ErrTooManySessions", attempt+1, err)
			}
		}

		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := infoDigests(infos), sortedDigests(IDDigest(first.ID), IDDigest(second.ID)); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("sessions = %v, want the untouched %v", got, want)
		}
		if got := eventKinds(events); len(got) != 2 || got[0] != EventCreated || got[1] != EventCreated {
			t.Fatalf("events = %v, want only the two creates", got)
		}
	})
}

// Zero, the default, means no limit at all.
func TestManagerCreateWithoutASessionLimitIsUnlimited(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.managerWithConfig(t, nil)
		for range 5 {
			if _, err := m.Create(ctx, "alice", nil); err != nil {
				t.Fatal(err)
			}
		}
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil || len(infos) != 5 {
			t.Fatalf("sessions = %v, %v; want 5", infos, err)
		}
	})
}

// A configured limit without a subject index fails closed: construction refuses
// the store, and a directly assembled manager reports ErrNotSupported from
// Create instead of storing the session.
func TestSessionLimitFailsClosedWithoutAnIndex(t *testing.T) {
	ctx := context.Background()
	plain := &recordingStore{}
	now := time.Unix(1_700_000_000, 0).UTC()

	m := testManagerWithConfig(t, plain, func() time.Time { return now }, func(c *Config) { c.MaxSessions = 1 })
	if _, err := m.Create(ctx, "alice", nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Create with a limit and a plain Store = %v, want ErrNotSupported", err)
	}

	cfg := DefaultConfig()
	cfg.MaxSessions = 1
	if _, err := New(cfg, WithStore(plain)); err == nil {
		t.Fatal("New() accepted max_sessions with a Store that has no index")
	}

	// The built-in memory store does implement the index, so the same
	// configuration is accepted and enforced.
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if _, err := p.Manager().Create(ctx, "alice", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Manager().Create(ctx, "alice", nil); err != nil {
		t.Fatal(err)
	}
	infos, err := p.Manager().ListBySubject(ctx, "alice")
	if err != nil || len(infos) != 1 {
		t.Fatalf("sessions = %v, %v; want 1 after the eviction", infos, err)
	}
}

// Rotate replaces a session rather than adding one, so the limit neither
// applies to it nor fails it.
func TestManagerRotateDoesNotApplyTheSessionLimit(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.managerWithConfig(t, func(c *Config) { c.MaxSessions = 1 })
		created, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		rotated, err := m.Rotate(ctx, created.ID)
		if err != nil {
			t.Fatalf("Rotate under a full limit = %v", err)
		}
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil || len(infos) != 1 || infos[0].Digest != IDDigest(rotated.ID) {
			t.Fatalf("sessions = %v, %v; want only the replacement", infos, err)
		}
	})
}

// Parallel Creates are best effort: they may exceed the limit transiently, and
// the next Create converges the indexed count back to it.
func TestManagerConcurrentCreatesConvergeToTheLimit(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.managerWithConfig(t, func(c *Config) { c.MaxSessions = 2 })

		const writers = 8
		var wg sync.WaitGroup
		failures := make(chan error, writers)
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.Create(ctx, "alice", nil); err != nil {
					failures <- err
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Fatalf("concurrent Create = %v, want a best-effort success", err)
		}

		if _, err := m.Create(ctx, "alice", nil); err != nil {
			t.Fatal(err)
		}
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		// Convergence means the next Create leaves the indexed count at or
		// below the limit. The distributed backend can transiently lose an
		// index member to the documented prune race, so the exact count is only
		// pinned for the store that writes session and index under one lock.
		if len(infos) == 0 || len(infos) > 2 {
			t.Fatalf("sessions after convergence = %v, want at most 2", infoDigests(infos))
		}
		if h.exact && len(infos) != 2 {
			t.Fatalf("sessions after convergence = %v, want exactly 2", infoDigests(infos))
		}
		for _, info := range infos {
			if !h.live(info.Digest) {
				t.Fatalf("ListBySubject reported dead session %s", info.Digest[:8])
			}
		}
	})
}
