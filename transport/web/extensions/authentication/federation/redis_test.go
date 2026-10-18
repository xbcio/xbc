package federation

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	store, err := NewRedisStore(client, "test:fed:")
	if err != nil {
		t.Fatal(err)
	}
	store.now = fixedClock(now)
	return server, client, store
}

// commandRecorder observes the Redis commands a client actually issues.
type commandRecorder struct {
	mu       sync.Mutex
	commands []string
}

func (r *commandRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, name)
}

func (r *commandRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func (r *commandRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = nil
}

func (r *commandRecorder) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (r *commandRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		r.record(cmd.Name())
		return next(ctx, cmd)
	}
}

func (r *commandRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestRedisConsumeScriptReadsAndDeletesInOneScript(t *testing.T) {
	if !strings.Contains(redisConsumeSource, "redis.call('GET'") || !strings.Contains(redisConsumeSource, "redis.call('DEL'") {
		t.Fatalf("redisConsumeSource must both read and delete, got %q", redisConsumeSource)
	}
	if calls := strings.Count(redisConsumeSource, "redis.call("); calls != 2 {
		t.Fatalf("redisConsumeSource issues %d redis calls, want exactly the GET and the DEL", calls)
	}
}

func TestRedisStoreConsumeIssuesTheScriptInOneRoundTrip(t *testing.T) {
	now := testTime(t)
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	recorder := &commandRecorder{}
	client.AddHook(recorder)
	store, err := NewRedisStore(client, "test:fed:")
	if err != nil {
		t.Fatal(err)
	}
	store.now = fixedClock(&now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	recorder.reset()
	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	sawScript := false
	for _, name := range recorder.names() {
		switch name {
		case "evalsha", "eval":
			sawScript = true
		case "get", "del":
			t.Fatalf("Consume issued a standalone %q command; the read and the delete must stay in one script", name)
		}
	}
	if !sawScript {
		t.Fatalf("Consume issued %v, want the consume script", recorder.names())
	}
}

func TestRedisStoreCreatesWithTTLAndANonReplayablePayload(t *testing.T) {
	now := testTime(t)
	server, _, store := newRedisStoreTest(t, &now)
	value := testTransaction(now, 5*time.Minute)
	if err := store.Create(context.Background(), value); err != nil {
		t.Fatal(err)
	}

	key := "test:fed:txn:" + testProvider + ":" + testState
	if !server.Exists(key) {
		t.Fatalf("Redis keys = %v, want %q", server.Keys(), key)
	}
	if ttl := server.TTL(key); ttl != 5*time.Minute {
		t.Fatalf("TTL = %s, want 5m", ttl)
	}
	payload, err := server.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, testSecret) {
		t.Fatal("stored payload contains the raw browser-binding secret")
	}
	if !strings.Contains(payload, BrowserBinding(testSecret)) {
		t.Fatal("stored payload does not contain the binding digest")
	}
}

func TestRedisStoreConsumeIsSingleUseAndDeletesTheKey(t *testing.T) {
	now := testTime(t)
	server, _, store := newRedisStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	key := "test:fed:txn:" + testProvider + ":" + testState

	got, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret))
	if err != nil || !found {
		t.Fatalf("Consume() found = %v, err = %v", found, err)
	}
	if got.Nonce != "nonce-value" || got.CodeVerifier != "pkce-verifier-value" || got.RequestID != "saml-request-id" || got.ReturnTo != "/dashboard" {
		t.Fatalf("Consume() = %#v", got)
	}
	if server.Exists(key) {
		t.Fatal("Consume() left the transaction key behind")
	}
	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err != nil || found {
		t.Fatalf("second Consume() found = %v, err = %v", found, err)
	}
}

func TestRedisStoreRejectsOtherBrowsersAndDestroysTheTransaction(t *testing.T) {
	now := testTime(t)
	server, _, store := newRedisStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding("other-browser")); err != nil || found {
		t.Fatalf("Consume(other browser) found = %v, err = %v", found, err)
	}
	if server.Exists("test:fed:txn:" + testProvider + ":" + testState) {
		t.Fatal("a mismatched browser left the transaction replayable")
	}
	if _, found, _ := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); found {
		t.Fatal("Consume() found a transaction a different browser already presented")
	}
}

func TestRedisStoreConcurrentConsumeOnlyOneWins(t *testing.T) {
	now := testTime(t)
	_, _, store := newRedisStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	const attempts = 16
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

func TestRedisStoreCreateRejectsDuplicatesAndInvalidLifetimes(t *testing.T) {
	now := testTime(t)
	_, _, store := newRedisStoreTest(t, &now)
	value := testTransaction(now, time.Minute)
	if err := store.Create(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), value); !errors.Is(err, ErrTransactionExists) {
		t.Fatalf("Create(duplicate) = %v, want ErrTransactionExists", err)
	}

	expired := testTransaction(now, time.Minute)
	expired.State = "fedcba9876543210fedcba9876543210"
	expired.ExpiresAt = now
	if err := store.Create(context.Background(), expired); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("Create(expired) = %v, want ErrInvalidTransaction", err)
	}

	subMillisecond := testTransaction(now, time.Minute)
	subMillisecond.State = "aaaabbbbccccddddeeeeffff00001111"
	subMillisecond.ExpiresAt = now.Add(500 * time.Microsecond)
	if err := store.Create(context.Background(), subMillisecond); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("Create(sub-millisecond lifetime) = %v, want ErrInvalidTransaction", err)
	}

	tooLong := testTransaction(now, time.Minute)
	tooLong.State = "11112222333344445555666677778888"
	tooLong.ExpiresAt = now.Add(MaxTransactionTTL + time.Second)
	if err := store.Create(context.Background(), tooLong); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("Create(overlong lifetime) = %v, want ErrInvalidTransaction", err)
	}
}

func TestRedisStoreHonorsTheApplicationClockBeforeRedisExpiry(t *testing.T) {
	now := testTime(t)
	server, _, store := newRedisStoreTest(t, &now)
	if err := store.Create(context.Background(), testTransaction(now, time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The key is still present in Redis, but the transaction's own deadline
	// has passed: Consume must still miss.
	now = now.Add(2 * time.Minute)
	if !server.Exists("test:fed:txn:" + testProvider + ":" + testState) {
		t.Fatal("test setup expected the Redis key to outlive the application deadline")
	}
	if _, found, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); err != nil || found {
		t.Fatalf("Consume(expired) found = %v, err = %v", found, err)
	}
}

func TestRedisStoreSetIfAbsentReservesUntilExpiry(t *testing.T) {
	now := testTime(t)
	server, _, store := newRedisStoreTest(t, &now)
	created, err := store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || !created {
		t.Fatalf("SetIfAbsent() created = %v, err = %v", created, err)
	}
	key := "test:fed:replay:saml:assertion-id"
	if !server.Exists(key) {
		t.Fatalf("Redis keys = %v, want %q", server.Keys(), key)
	}
	created, err = store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || created {
		t.Fatalf("second SetIfAbsent() created = %v, err = %v", created, err)
	}

	server.FastForward(2 * time.Minute)
	created, err = store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute)
	if err != nil || !created {
		t.Fatalf("SetIfAbsent() after expiry created = %v, err = %v", created, err)
	}
}

func TestRedisStoreSetIfAbsentValidatesReservations(t *testing.T) {
	now := testTime(t)
	_, _, store := newRedisStoreTest(t, &now)
	for name, testCase := range map[string]struct {
		namespace string
		key       string
		ttl       time.Duration
	}{
		"uppercase namespace": {"SAML", "assertion-id", time.Minute},
		"empty key":           {"saml", "", time.Minute},
		"key with colon":      {"saml", "a:b", time.Minute},
		"zero ttl":            {"saml", "assertion-id", 0},
		"ttl above the bound": {"saml", "assertion-id", MaxReservationTTL + time.Second},
		"sub-millisecond ttl": {"saml", "assertion-id", 500 * time.Microsecond},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.SetIfAbsent(context.Background(), testCase.namespace, testCase.key, testCase.ttl); !errors.Is(err, ErrInvalidReservation) {
				t.Fatalf("SetIfAbsent() = %v, want ErrInvalidReservation", err)
			}
		})
	}
}

func TestNewRedisStoreRejectsNilClientsAndBlankPrefixes(t *testing.T) {
	if _, err := NewRedisStore(nil, "test:"); err == nil {
		t.Fatal("NewRedisStore(nil) = nil error")
	}
	var typedNil *goredis.Client
	if _, err := NewRedisStore(typedNil, "test:"); err == nil {
		t.Fatal("NewRedisStore(typed nil) = nil error")
	}
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	for name, prefix := range map[string]string{"empty": "", "blank": "  ", "control": "test:\n"} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRedisStore(client, prefix); err == nil {
				t.Fatalf("NewRedisStore(prefix %q) = nil error", prefix)
			}
		})
	}
}

func TestRedisStoreWrapsBackendFailures(t *testing.T) {
	now := testTime(t)
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	store, err := NewRedisStore(client, "test:fed:")
	if err != nil {
		t.Fatal(err)
	}
	store.now = fixedClock(&now)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.Create(context.Background(), testTransaction(now, time.Minute)); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("Create() = %v, want ErrTransactionStore", err)
	}
	if _, _, err := store.Consume(context.Background(), testProvider, testState, BrowserBinding(testSecret)); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("Consume() = %v, want ErrTransactionStore", err)
	}
	if _, err := store.SetIfAbsent(context.Background(), "saml", "assertion-id", time.Minute); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("SetIfAbsent() = %v, want ErrTransactionStore", err)
	}
}

func TestRedisStoreRejectsNilContextsAndCancellation(t *testing.T) {
	now := testTime(t)
	_, _, store := newRedisStoreTest(t, &now)
	if err := store.Create(nil, testTransaction(now, time.Minute)); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Create(nil) = %v", err)
	}
	if _, _, err := store.Consume(nil, testProvider, testState, BrowserBinding(testSecret)); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Consume(nil) = %v", err)
	}
	if _, err := store.SetIfAbsent(nil, "saml", "assertion-id", time.Minute); !errors.Is(err, ErrNilContext) {
		t.Fatalf("SetIfAbsent(nil) = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Create(ctx, testTransaction(now, time.Minute)); !errors.Is(err, ErrTransactionStore) {
		t.Fatalf("Create(cancelled) = %v, want ErrTransactionStore", err)
	}
}

func TestRedisStoreNilReceiversReportErrNilStore(t *testing.T) {
	var store *RedisStore
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
