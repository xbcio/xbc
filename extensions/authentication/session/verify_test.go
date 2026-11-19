package session

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
)

// Verify classifies every outcome: a live session is Valid, a well-formed ID
// the store does not know is NotFound, a wrong-shaped ID is Malformed without
// touching the store, a backend failure is StoreError with the error, and a
// stopped manager is StoreError with ErrStopped.
func TestManagerVerifyClassifiesEveryOutcome(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)
		created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
		if err != nil {
			t.Fatal(err)
		}
		value, verdict, err := m.Verify(ctx, created.ID)
		if err != nil || verdict != VerdictValid || value.ID != created.ID || value.Subject != "alice" {
			t.Fatalf("Verify() = %#v, %v, %v; want valid", value, verdict, err)
		}
		if value.Attributes["role"] != "admin" {
			t.Fatalf("Verify() attributes = %#v", value.Attributes)
		}
		// The result is a defensive copy.
		value.Attributes["role"] = "mutated"
		stored, found, err := h.store.Get(ctx, created.ID)
		if err != nil || !found || stored.Attributes["role"] != "admin" {
			t.Fatalf("Verify leaked a mutable reference: %#v, %v, %v", stored.Attributes, found, err)
		}

		if _, verdict, err := m.Verify(ctx, testID(80, 32)); err != nil || verdict != VerdictNotFound {
			t.Fatalf("Verify(unknown) verdict=%v err=%v, want NotFound", verdict, err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		store := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
			t.Fatal("Verify must reject a wrong-shaped ID before touching the store")
			return Session{}, false, nil
		}}
		m := testManager(t, store, time.Unix(1_700_000_000, 0).UTC())
		if _, verdict, err := m.Verify(context.Background(), "attacker-selected"); err != nil || verdict != VerdictMalformed {
			t.Fatalf("Verify(malformed) verdict=%v err=%v, want Malformed", verdict, err)
		}
	})

	t.Run("store error", func(t *testing.T) {
		backendErr := errors.New("backend details")
		store := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
			return Session{}, false, backendErr
		}}
		m := testManager(t, store, time.Unix(1_700_000_000, 0).UTC())
		_, verdict, err := m.Verify(context.Background(), testID(81, 32))
		if verdict != VerdictStoreError || !errors.Is(err, backendErr) {
			t.Fatalf("Verify(store error) verdict=%v err=%v, want StoreError with the backend error", verdict, err)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		m := testManager(t, &recordingStore{}, time.Unix(1_700_000_000, 0).UTC())
		m.stop()
		_, verdict, err := m.Verify(context.Background(), testID(82, 32))
		if verdict != VerdictStoreError || !errors.Is(err, ErrStopped) {
			t.Fatalf("Verify(stopped) verdict=%v err=%v, want StoreError with ErrStopped", verdict, err)
		}
	})

	t.Run("nil context", func(t *testing.T) {
		m := testManager(t, &recordingStore{}, time.Unix(1_700_000_000, 0).UTC())
		_, verdict, err := m.Verify(nil, testID(83, 32))
		if verdict != VerdictStoreError || err == nil {
			t.Fatalf("Verify(nil context) verdict=%v err=%v, want StoreError with an error", verdict, err)
		}
	})
}

// The zero Verdict is a refusal, so an unassigned classification never reads
// as a valid session.
func TestVerdictZeroValueNeverReadsAsValid(t *testing.T) {
	var verdict Verdict
	if verdict == VerdictValid {
		t.Fatal("the zero Verdict reads as valid")
	}
	if verdict.String() != "malformed" {
		t.Fatalf("zero Verdict String() = %q", verdict.String())
	}
	for want, value := range map[string]Verdict{
		"valid":       VerdictValid,
		"malformed":   VerdictMalformed,
		"not_found":   VerdictNotFound,
		"store_error": VerdictStoreError,
	} {
		if value.String() != want {
			t.Fatalf("Verdict(%d).String() = %q, want %q", int(value), value.String(), want)
		}
	}
}

// An expired, a revoked, and a never-created session must be indistinguishable
// through Verify: same verdict, same zero session, no error.
func TestManagerVerifyReportsDeadSessionsIdentically(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)

		expired, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		revoked, err := m.Create(ctx, "bob", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Revoke(ctx, revoked.ID); err != nil {
			t.Fatal(err)
		}
		h.advance(25 * time.Hour)

		for name, id := range map[string]string{
			"expired":       expired.ID,
			"revoked":       revoked.ID,
			"never created": testID(84, 32),
		} {
			value, verdict, err := m.Verify(ctx, id)
			if err != nil || verdict != VerdictNotFound {
				t.Fatalf("%s Verify() = %#v, %v, %v; want an identical NotFound", name, value, verdict, err)
			}
			if !reflect.DeepEqual(value, Session{}) {
				t.Fatalf("%s Verify() returned a session: %#v", name, value)
			}
		}
	})
}

// Verify renews the idle lease exactly as Authenticate did: not before
// touch_interval has elapsed, and the renewed lease is what keeps the session
// alive afterwards.
func TestManagerVerifyRenewsIdleLeaseOnlyPastTouchInterval(t *testing.T) {
	forEachIndexedStore(t, func(t *testing.T, h *indexHarness) {
		ctx := context.Background()
		m := h.manager(t)
		created, err := m.Create(ctx, "alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		lastSeen := func() time.Time {
			t.Helper()
			infos, err := m.ListBySubject(ctx, "alice")
			if err != nil || len(infos) != 1 {
				t.Fatalf("ListBySubject() = %v, %v", infos, err)
			}
			return infos[0].LastSeenAt
		}
		first := lastSeen()
		if !first.Equal(created.IssuedAt) {
			t.Fatalf("LastSeenAt = %v, want the issuance time %v", first, created.IssuedAt)
		}

		h.advance(2 * time.Minute)
		if _, verdict, err := m.Verify(ctx, created.ID); err != nil || verdict != VerdictValid {
			t.Fatalf("Verify() verdict=%v err=%v", verdict, err)
		}
		if got := lastSeen(); !got.Equal(first) {
			t.Fatalf("LastSeenAt moved inside touch_interval: %v -> %v", first, got)
		}

		h.advance(4 * time.Minute)
		if _, verdict, err := m.Verify(ctx, created.ID); err != nil || verdict != VerdictValid {
			t.Fatalf("Verify() after touch_interval verdict=%v err=%v", verdict, err)
		}
		renewed := lastSeen()
		if !renewed.Equal(*h.now) {
			t.Fatalf("LastSeenAt = %v, want the renewal time %v", renewed, *h.now)
		}

		h.advance(29 * time.Minute)
		infos, err := m.ListBySubject(ctx, "alice")
		if err != nil || len(infos) != 1 {
			t.Fatalf("List() before the renewed lease ends = %v, %v; want the session alive", infos, err)
		}
		h.advance(2 * time.Minute)
		if infos, err := m.ListBySubject(ctx, "alice"); err != nil || len(infos) != 0 {
			t.Fatalf("List() after the renewed lease ends = %v, %v; want no session", infos, err)
		}
		if _, verdict, err := m.Verify(ctx, created.ID); err != nil || verdict != VerdictNotFound {
			t.Fatalf("Verify() after the idle lease ended verdict=%v err=%v, want NotFound", verdict, err)
		}
	})
}

// Authenticate's external contract is unchanged: shape, unknown, expired,
// store errors, and a stopped plugin all produce the exact same rejection,
// even though Verify now distinguishes them internally.
func TestPluginAuthenticateCollapsesEveryVerdictToTheSameResult(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	var baseline *authentication.Result
	record := func(name string, result authentication.Result, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s Authenticate() error = %v", name, err)
		}
		if !result.Rejected() {
			t.Fatalf("%s Authenticate() was not rejected", name)
		}
		reason, ok := result.Reason()
		if !ok || reason != reasonInvalidCredential || len(result.Challenges()) != 0 {
			t.Fatalf("%s Authenticate() rejection = %q, challenges %v", name, reason, result.Challenges())
		}
		if baseline == nil {
			baseline = &result
			return
		}
		if !reflect.DeepEqual(*baseline, result) {
			t.Fatalf("%s Authenticate() result differs from the collapsed baseline: %#v vs %#v", name, result, *baseline)
		}
	}

	rejections := map[string]func(t *testing.T) (*Plugin, authentication.Credential){
		"malformed": func(t *testing.T) (*Plugin, authentication.Credential) {
			return configuredSessionPlugin(t, &recordingStore{}, nil), presentedSessionCredential(t, "not-an-opaque-id")
		},
		"unknown": func(t *testing.T) (*Plugin, authentication.Credential) {
			store := newControlledMemoryStore(t, &now)
			return configuredSessionPlugin(t, store, nil), presentedSessionCredential(t, testID(90, 32))
		},
		"expired": func(t *testing.T) (*Plugin, authentication.Credential) {
			localNow := now
			store := newControlledMemoryStore(t, &localNow)
			id := testID(91, 32)
			if err := store.Create(context.Background(), sessionValue(id, "alice", localNow, time.Minute), time.Minute); err != nil {
				t.Fatal(err)
			}
			localNow = localNow.Add(2 * time.Minute)
			return configuredSessionPlugin(t, store, nil), presentedSessionCredential(t, id)
		},
		"store error": func(t *testing.T) (*Plugin, authentication.Credential) {
			store := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
				return Session{}, false, errors.New("backend details")
			}}
			return configuredSessionPlugin(t, store, nil), presentedSessionCredential(t, testID(92, 32))
		},
		"stopped": func(t *testing.T) (*Plugin, authentication.Credential) {
			p := configuredSessionPlugin(t, &recordingStore{}, nil)
			if err := p.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			return p, presentedSessionCredential(t, testID(93, 32))
		},
	}
	for name, setup := range rejections {
		p, credential := setup(t)
		result, err := p.Authenticate(context.Background(), credential)
		record(name, result, err)
	}
}
