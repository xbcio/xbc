package distributed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestSemaphore builds a semaphore over client with a discarded logger, so
// a test that deliberately loses a lease does not write diagnostics into the
// test output.
func newTestSemaphore(t *testing.T, client redis.UniversalClient, options SemaphoreOptions) *Semaphore {
	t.Helper()
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	created, err := NewSemaphore(client, options)
	if err != nil {
		t.Fatalf("NewSemaphore(): %v", err)
	}
	return created
}

func TestSemaphoreRunRefusesWhenFull(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- semaphore.Run(ctx, "work", 1, time.Second, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first execution did not take the only slot")
	}

	entered := false
	err := semaphore.Run(ctx, "work", 1, time.Second, func(context.Context) error {
		entered = true
		return nil
	})
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("Run() while the slot is held = %v, want ErrLimitReached", err)
	}
	if entered {
		t.Fatal("the refused execution ran its callback")
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("the admitted execution returned %v", err)
	}
	if card := client.ZCard(ctx, "distributed:sem:work").Val(); card != 0 {
		t.Fatalf("slots left after every execution returned = %d, want 0", card)
	}
}

func TestSemaphoreRecoversExpiredSlots(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()
	key := "distributed:sem:work"

	// A replica that died holds the only slot. Nobody will ever release it.
	expired := float64(time.Now().Add(-time.Minute).UnixMilli())
	if err := client.ZAdd(ctx, key, redis.Z{Score: expired, Member: "dead-token"}).Err(); err != nil {
		t.Fatalf("seed expired token error = %v", err)
	}

	called := false
	if err := semaphore.Run(ctx, "work", 1, time.Second, func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Run() over an expired slot = %v", err)
	}
	if !called {
		t.Fatal("the execution did not run after the stale slot expired")
	}
	if members := client.ZRange(ctx, key, 0, -1).Val(); len(members) != 0 {
		t.Fatalf("slots after the execution returned = %v, want none", members)
	}
}

func TestSemaphoreRecoversAForeignRedisType(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()
	key := "distributed:sem:work"

	if err := client.Set(ctx, key, "claimed by another writer", 0).Err(); err != nil {
		t.Fatalf("seed foreign key error = %v", err)
	}
	if err := semaphore.Run(ctx, "work", 1, time.Second, func(ctx context.Context) error {
		if typ := client.Type(ctx, key).Val(); typ != "zset" {
			t.Errorf("key type during the execution = %q, want zset", typ)
		}
		if card := client.ZCard(ctx, key).Val(); card != 1 {
			t.Errorf("recovered key card during the execution = %d, want the new slot", card)
		}
		return nil
	}); err != nil {
		t.Fatalf("Run() over a foreign type = %v", err)
	}
}

func TestSemaphoreRenewalLossCancelsTheCallbackContext(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()
	key := "distributed:sem:work"

	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- semaphore.Run(ctx, "work", 1, 150*time.Millisecond, func(runCtx context.Context) error {
			close(started)
			<-runCtx.Done()
			return runCtx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the execution did not start")
	}
	// Remove the slot as a reset or an operator would; the renewal loop then
	// observes that the token is gone.
	if err := client.Del(ctx, key).Err(); err != nil {
		t.Fatalf("delete semaphore key error = %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() = %v, want the cancelled context", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a lost lease did not cancel the callback context")
	}
}

func TestSemaphoreResetKeepsLiveSlots(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()
	key := "distributed:sem:work"

	expired := float64(time.Now().Add(-time.Minute).UnixMilli())
	live := float64(time.Now().Add(time.Minute).UnixMilli())
	if err := client.ZAdd(ctx, key,
		redis.Z{Score: expired, Member: "expired-token"},
		redis.Z{Score: live, Member: "live-token"},
	).Err(); err != nil {
		t.Fatalf("seed tokens error = %v", err)
	}
	remaining, err := semaphore.Reset(ctx, "work")
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if remaining != 1 {
		t.Fatalf("Reset() = %d live slots, want 1", remaining)
	}
	if members := client.ZRange(ctx, key, 0, -1).Val(); len(members) != 1 || members[0] != "live-token" {
		t.Fatalf("members after Reset() = %v, want only the live slot", members)
	}

	// A key claimed by a foreign type is dropped rather than left wedged.
	if err := client.Set(ctx, "distributed:sem:foreign", "junk", 0).Err(); err != nil {
		t.Fatalf("seed foreign key error = %v", err)
	}
	remaining, err = semaphore.Reset(ctx, "foreign")
	if err != nil {
		t.Fatalf("Reset(foreign) error = %v", err)
	}
	if remaining != 0 {
		t.Fatalf("Reset(foreign) = %d, want 0", remaining)
	}
	if typ := client.Type(ctx, "distributed:sem:foreign").Val(); typ != "none" {
		t.Fatalf("foreign key type after Reset() = %q, want none", typ)
	}
}

func TestSemaphoreRejectsInvalidParameters(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx := context.Background()
	noop := func(context.Context) error {
		t.Fatal("an invalid call ran its callback")
		return nil
	}

	for name, testCase := range map[string]struct {
		semaphoreName string
		limit         int
		lease         time.Duration
		want          error
	}{
		"empty name":            {semaphoreName: "", limit: 1, lease: time.Second, want: ErrInvalidName},
		"leading space in name": {semaphoreName: " work", limit: 1, lease: time.Second, want: ErrInvalidName},
		"trailing space in name": {
			semaphoreName: "work ", limit: 1, lease: time.Second, want: ErrInvalidName,
		},
		"zero limit": {semaphoreName: "work", limit: 0, lease: time.Second, want: ErrInvalidOption},
		"negative limit": {
			semaphoreName: "work", limit: -1, lease: time.Second, want: ErrInvalidOption,
		},
		"zero lease": {semaphoreName: "work", limit: 1, lease: 0, want: ErrInvalidOption},
		"negative lease": {
			semaphoreName: "work", limit: 1, lease: -time.Nanosecond, want: ErrInvalidOption,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := semaphore.Run(ctx, testCase.semaphoreName, testCase.limit, testCase.lease, noop)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("Run() error = %v, want %v", err, testCase.want)
			}
		})
	}
	if names := client.Keys(ctx, "distributed:sem:*").Val(); len(names) != 0 {
		t.Fatalf("invalid calls created keys %v", names)
	}
}

func TestNewSemaphoreRejectsInvalidConfiguration(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	if _, err := NewSemaphore(nil, SemaphoreOptions{}); err == nil {
		t.Fatal("NewSemaphore(nil) succeeded, want an error")
	}
	if _, err := NewSemaphore(client, SemaphoreOptions{Prefix: "Has Space"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("NewSemaphore(malformed prefix) = %v, want ErrInvalidOption", err)
	}
}

func TestSemaphorePrefixNamesTheRedisKey(t *testing.T) {
	for name, testCase := range map[string]struct {
		prefix string
		key    string
	}{
		"default prefix": {prefix: "", key: "distributed:sem:work"},
		"custom prefix":  {prefix: "rules", key: "rules:sem:work"},
	} {
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := newTestClient(t, server)
			semaphore := newTestSemaphore(t, client, SemaphoreOptions{Prefix: testCase.prefix})
			ctx := context.Background()

			if err := semaphore.Run(ctx, "work", 1, time.Second, func(ctx context.Context) error {
				if typ := client.Type(ctx, testCase.key).Val(); typ != "zset" {
					t.Errorf("key %q type during the execution = %q, want zset", testCase.key, typ)
				}
				if testCase.prefix != "" {
					if exists := client.Exists(ctx, "distributed:sem:work").Val(); exists != 0 {
						t.Errorf("default key exists under a custom prefix")
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
		})
	}
}

func TestSemaphoreTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		token := newSemaphoreToken("work")
		if token == "" {
			t.Fatal("newSemaphoreToken returned an empty token")
		}
		if seen[token] {
			t.Fatalf("newSemaphoreToken returned %q twice", token)
		}
		seen[token] = true
	}
}

func TestLeaseMillisRoundsUpSubMillisecondLeases(t *testing.T) {
	for name, testCase := range map[string]struct {
		lease time.Duration
		want  int64
	}{
		"a sub-millisecond lease becomes one millisecond": {lease: 500 * time.Microsecond, want: 1},
		"a zero lease does not expire immediately":        {lease: 0, want: 1},
		"a whole lease keeps its milliseconds":            {lease: 2500 * time.Microsecond, want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if got := leaseMillis(testCase.lease); got != testCase.want {
				t.Fatalf("leaseMillis(%s) = %d, want %d", testCase.lease, got, testCase.want)
			}
		})
	}
}

func TestSemaphoreRunHonorsAnAlreadyCancelledContext(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	semaphore := newTestSemaphore(t, client, SemaphoreOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := semaphore.Run(ctx, "work", 1, time.Second, func(context.Context) error {
		t.Fatal("a cancelled call ran its callback")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() with a cancelled context = %v, want context.Canceled", err)
	}
	if names := client.Keys(context.Background(), "distributed:sem:*").Val(); len(names) != 0 {
		t.Fatalf("a cancelled call created keys %v", names)
	}
}
