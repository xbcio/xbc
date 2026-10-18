package federation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newMemoryStoreTest(t *testing.T, now *time.Time) *MemoryStore {
	t.Helper()
	store := NewMemoryStore()
	store.now = fixedClock(now)
	return store
}

func TestMemoryStoreConsumeIsSingleUse(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	value := testTransaction(now, 5*time.Minute)
	if err := store.Create(context.Background(), value); err != nil {
		t.Fatal(err)
	}

	got, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret))
	if err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if got.Nonce != value.Nonce || got.CodeVerifier != value.CodeVerifier || got.RequestID != value.RequestID || got.ReturnTo != value.ReturnTo {
		t.Fatalf("Consume() = %#v, want the stored transaction", got)
	}

	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err != nil || found {
		t.Fatalf("second Consume() found = %v, err = %v; want a miss", found, err)
	}
}

func TestMemoryStoreRejectsOtherBrowsersAndDestroysTheTransaction(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding("other-browser")); err != nil || found {
		t.Fatalf("Consume(other browser) found = %v, err = %v", found, err)
	}
	// The mismatched attempt destroyed the transaction, so the browser that
	// started the login cannot complete it either: a callback that reached the
	// wrong browser is spent, not retryable.
	if _, found, _ := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); found {
		t.Fatal("Consume() found a transaction that a different browser already presented")
	}
}

func TestMemoryStoreHonorsExpiry(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err != nil || found {
		t.Fatalf("Consume(expired) found = %v, err = %v", found, err)
	}
}

func TestMemoryStoreCreateRejectsDuplicateLiveStateAndAllowsReuseAfterExpiry(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	value := testTransaction(now, time.Minute)
	if err := store.Create(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), value); !errors.Is(err, ErrTransactionExists) {
		t.Fatalf("Create(duplicate) = %v, want ErrTransactionExists", err)
	}

	now = now.Add(2 * time.Minute)
	if err := store.Create(context.Background(), testTransaction(now, time.Minute)); err != nil {
		t.Fatalf("Create() after expiry error = %v", err)
	}
}

func TestMemoryStoreCreateValidatesTheTransaction(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	valid := testTransaction(now, time.Minute)
	for name, mutate := range map[string]func(*Transaction){
		"provider":          func(tx *Transaction) { tx.Provider = "../escape" },
		"short state":       func(tx *Transaction) { tx.State = "short" },
		"state separator":   func(tx *Transaction) { tx.State = "0123456789abcdef:0123456789abcdef" },
		"binding":           func(tx *Transaction) { tx.Binding = "not-a-digest" },
		"return_to control": func(tx *Transaction) { tx.ReturnTo = "/ok\r\n" },
		"oversized nonce":   func(tx *Transaction) { tx.Nonce = strings.Repeat("a", maxMaterialLength+1) },
		"expired":           func(tx *Transaction) { tx.ExpiresAt = now },
		"lifetime too long": func(tx *Transaction) { tx.ExpiresAt = now.Add(MaxTransactionTTL + time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			tx := valid
			mutate(&tx)
			if err := store.Create(context.Background(), tx); !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("Create() = %v, want ErrInvalidTransaction", err)
			}
		})
	}
}

func TestMemoryStoreConsumeTreatsMalformedMaterialAsAMiss(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	for name, testCase := range map[string]struct{ provider, state, binding string }{
		"provider":        {"../escape", testState, BrowserBinding(testSecret)},
		"short state":     {testProvider, "short", BrowserBinding(testSecret)},
		"state separator": {testProvider, "0123456789abcdef:0123456789abcdef", BrowserBinding(testSecret)},
		"binding":         {testProvider, testState, "not-a-digest"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, found, err := store.Consume(context.Background(), testCase.provider, testCase.state, testCase.binding); err != nil || found {
				t.Fatalf("Consume() found = %v, err = %v; want a miss without an error", found, err)
			}
		})
	}
}

func TestMemoryStoreConcurrentConsumeOnlyOneWins(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	const attempts = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err == nil && found {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("concurrent Consume winners = %d, want exactly 1", winners)
	}
}

func TestMemoryStoreSetIfAbsentReservesUntilExpiry(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	created, err := store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || !created {
		t.Fatalf("SetIfAbsent() created = %v, err = %v", created, err)
	}
	created, err = store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || created {
		t.Fatalf("second SetIfAbsent() created = %v, err = %v", created, err)
	}

	now = now.Add(2 * time.Minute)
	created, err = store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || !created {
		t.Fatalf("SetIfAbsent() after expiry created = %v, err = %v", created, err)
	}
}

func TestMemoryStoreSetIfAbsentValidatesReservations(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	for name, testCase := range map[string]struct {
		namespace string
		key       string
		ttl       time.Duration
	}{
		"uppercase namespace": {"SAML", "assertion-id", time.Minute},
		"namespace separator": {"saml:x", "assertion-id", time.Minute},
		"empty key":           {"saml", "", time.Minute},
		"key with colon":      {"saml", "a:b", time.Minute},
		"zero ttl":            {"saml", "assertion-id", 0},
		"ttl above the bound": {"saml", "assertion-id", MaxReservationTTL + time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.SetIfAbsent(context.Background(), testCase.namespace, testCase.key, testCase.ttl); !errors.Is(err, ErrInvalidReservation) {
				t.Fatalf("SetIfAbsent() = %v, want ErrInvalidReservation", err)
			}
		})
	}
}

func TestMemoryStoreRejectsNilContexts(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	if err := store.Create(nil, testTransaction(now, time.Minute)); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Create(nil) = %v", err)
	}
	if _, _, err := store.Consume(nil, testProvider, testState, BrowserBinding(testSecret)); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Consume(nil) = %v", err)
	}
	if _, err := store.SetIfAbsent(nil, "saml", "assertion-id", time.Minute); !errors.Is(err, ErrNilContext) {
		t.Fatalf("SetIfAbsent(nil) = %v", err)
	}
}

func TestMemoryStoreReportsCancelledContexts(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Create(ctx, testTransaction(now, time.Minute)); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("Create(cancelled) = %v, want ErrTransactionStore", err)
	}
}

func TestMemoryStoreNilReceiversReportErrNilStore(t *testing.T) {
	var store *MemoryStore
	if err := store.Create(context.Background(), testTransaction(time.Now(), time.Minute)); !errors.Is(err, ErrNilStore) {
		t.Fatalf("Create() = %v", err)
	}
	if _, _, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); !errors.Is(err, ErrNilStore) {
		t.Fatalf("Consume() = %v", err)
	}
	if _, err := store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute); !errors.Is(err, ErrNilStore) {
		t.Fatalf("SetIfAbsent() = %v", err)
	}
}

func TestMemoryStoreSweepsExpiredEntriesOnWrite(t *testing.T) {
	now := testTime(t)
	store := newMemoryStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 {
		t.Fatalf("entries = %d", len(store.entries))
	}

	now = now.Add(2 * memorySweepInterval)
	other := testTransaction(now, time.Minute)
	other.State = "fedcba9876543210fedcba9876543210"
	if err := store.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 {
		t.Fatalf("entries after sweep = %d, want only the live transaction", len(store.entries))
	}
	if _, ok := store.entries[transactionKey("", other.Provider, other.State)]; !ok {
		t.Fatal("sweep removed the transaction it was called for")
	}
}
