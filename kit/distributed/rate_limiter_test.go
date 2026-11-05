package distributed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRateLimiter builds a rate limiter over client with a discarded
// logger, so a test that deliberately corrupts state does not write warnings
// into the test output.
func newTestRateLimiter(t *testing.T, client redis.UniversalClient, options RateLimiterOptions) *RateLimiter {
	t.Helper()
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	created, err := NewRateLimiter(client, options)
	if err != nil {
		t.Fatalf("NewRateLimiter(): %v", err)
	}
	return created
}

// freezeRedisTime pins the server clock, so reservation delays are decided by
// the GCRA state alone instead of by how long the test took to issue calls.
func freezeRedisTime(t *testing.T, server *miniredis.Miniredis) time.Time {
	t.Helper()
	epoch := time.Unix(1_700_000_000, 0)
	server.SetTime(epoch)
	return epoch
}

// waitWithin reports whether a returned delay is within the clock tolerance of
// the expected one. The limiter schedules in microseconds, so the tolerance is
// a bound on arithmetic error, not on scheduling policy.
func waitWithin(got, want, tolerance time.Duration) bool {
	difference := got - want
	if difference < 0 {
		difference = -difference
	}
	return difference <= tolerance
}

func TestRateLimiterLongTermRate(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)
	ctx := context.Background()

	// limit=5 per 1s means an emission interval of 200ms and a burst of 5.
	waits := make([]time.Duration, 10)
	for i := range waits {
		wait, err := limiter.Reserve(ctx, "render", 5, time.Second)
		if err != nil {
			t.Fatalf("Reserve() call %d: %v", i+1, err)
		}
		waits[i] = wait
	}

	for i := 0; i < 5; i++ {
		if waits[i] != 0 {
			t.Fatalf("reservation %d waited %s, want an immediate slot", i+1, waits[i])
		}
	}
	for i := 5; i < 10; i++ {
		want := time.Duration(i-4) * 200 * time.Millisecond
		if !waitWithin(waits[i], want, 2*time.Millisecond) {
			t.Fatalf("reservation %d waited %s, want about %s", i+1, waits[i], want)
		}
	}
}

func TestRateLimiterConcurrentReservationsDoNotOverAdmit(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)

	const callers = 50
	type reservation struct {
		wait time.Duration
		err  error
	}
	results := make(chan reservation, callers)
	var started sync.WaitGroup
	for range callers {
		started.Add(1)
		go func() {
			defer started.Done()
			wait, err := limiter.Reserve(context.Background(), "render", 10, time.Second)
			results <- reservation{wait: wait, err: err}
		}()
	}
	started.Wait()
	close(results)

	immediate := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("a concurrent Reserve() returned %v", result.err)
		}
		if result.wait == 0 {
			immediate++
		}
	}
	if immediate != 10 {
		t.Fatalf("%d of %d concurrent reservations were admitted immediately, want exactly 10", immediate, callers)
	}
}

func TestRateLimiterAllowDoesNotConsumeWhenItWouldWait(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	epoch := freezeRedisTime(t, server)
	ctx := context.Background()

	// limit=2 per 1s means a burst of 2 and then one slot every 500ms.
	for i := 0; i < 2; i++ {
		allowed, err := limiter.Allow(ctx, "render", 2, time.Second)
		if err != nil {
			t.Fatalf("Allow() call %d: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow() call %d was refused inside the burst", i+1)
		}
	}
	allowed, err := limiter.Allow(ctx, "render", 2, time.Second)
	if err != nil {
		t.Fatalf("Allow() while the burst is spent: %v", err)
	}
	if allowed {
		t.Fatal("Allow() admitted a third request inside a burst of two")
	}

	// The refused Allow must have left the schedule at one second: a Reserve
	// now waits half a second, not the full second a consumed slot would add.
	wait, err := limiter.Reserve(ctx, "render", 2, time.Second)
	if err != nil {
		t.Fatalf("Reserve() after the refused Allow: %v", err)
	}
	if !waitWithin(wait, 500*time.Millisecond, 2*time.Millisecond) {
		t.Fatalf("Reserve() after the refused Allow waited %s, want about 500ms: Allow consumed a slot", wait)
	}

	// Once the schedule catches up, Allow succeeds again.
	server.SetTime(epoch.Add(time.Second))
	allowed, err = limiter.Allow(ctx, "render", 2, time.Second)
	if err != nil {
		t.Fatalf("Allow() after the clock advanced: %v", err)
	}
	if !allowed {
		t.Fatal("Allow() refused a request after the schedule had passed")
	}
}

func TestRateLimiterFractionalEmissionInterval(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)
	ctx := context.Background()

	// limit=3 per 10ms gives an emission interval of 10/3ms. Truncating or
	// rounding it to whole milliseconds changes the 300th reservation's delay
	// by tens of milliseconds, so this pins the long-term rate.
	const reservations = 300
	var last time.Duration
	for i := 0; i < reservations; i++ {
		wait, err := limiter.Reserve(ctx, "render", 3, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("Reserve() call %d: %v", i+1, err)
		}
		last = wait
	}
	// The first three run immediately; the 300th runs (299 * 10/3ms - 20/3ms)
	// after the start, which is 990ms.
	want := 990 * time.Millisecond
	if !waitWithin(last, want, 3*time.Millisecond) {
		t.Fatalf("the 300th reservation waited %s, want about %s: the emission interval drifted", last, want)
	}
}

func TestRateLimiterWaitBlocksForTheReservedDelay(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)
	ctx := context.Background()

	if _, err := limiter.Reserve(ctx, "render", 1, 100*time.Millisecond); err != nil {
		t.Fatalf("Reserve() before Wait: %v", err)
	}
	started := time.Now()
	if err := limiter.Wait(ctx, "render", 1, 100*time.Millisecond); err != nil {
		t.Fatalf("Wait(): %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("Wait() returned after %s, before the reserved delay", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Wait() returned after %s, far beyond the reserved delay", elapsed)
	}
}

func TestRateLimiterWaitHonorsContextCancellation(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)

	if _, err := limiter.Reserve(context.Background(), "render", 1, time.Second); err != nil {
		t.Fatalf("Reserve() before Wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := limiter.Wait(ctx, "render", 1, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() with a deadline that expires during the wait = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Wait() returned after %s instead of when the context ended", elapsed)
	}
}

func TestRateLimiterKeyExpiresAndRefreshes(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)
	ctx := context.Background()
	key := "distributed:rate:render"

	if _, err := limiter.Reserve(ctx, "render", 1, time.Second); err != nil {
		t.Fatalf("Reserve(): %v", err)
	}
	if ttl := server.TTL(key); ttl != 2*time.Second {
		t.Fatalf("key ttl after the first reservation = %s, want twice per (2s)", ttl)
	}

	server.FastForward(1500 * time.Millisecond)
	if ttl := server.TTL(key); ttl != 500*time.Millisecond {
		t.Fatalf("key ttl after FastForward = %s, want 500ms", ttl)
	}
	if _, err := limiter.Reserve(ctx, "render", 1, time.Second); err != nil {
		t.Fatalf("Reserve() after FastForward: %v", err)
	}
	if ttl := server.TTL(key); ttl != 2*time.Second {
		t.Fatalf("key ttl after a reservation = %s, want twice per again: the write did not refresh the ttl", ttl)
	}
}

func TestRateLimiterPrefixNamesTheRedisKey(t *testing.T) {
	for name, testCase := range map[string]struct {
		prefix string
		key    string
	}{
		"default prefix": {prefix: "", key: "distributed:rate:render"},
		"custom prefix":  {prefix: "rules", key: "rules:rate:render"},
	} {
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := newTestClient(t, server)
			limiter := newTestRateLimiter(t, client, RateLimiterOptions{Prefix: testCase.prefix})
			freezeRedisTime(t, server)

			if _, err := limiter.Reserve(context.Background(), "render", 1, time.Second); err != nil {
				t.Fatalf("Reserve(): %v", err)
			}
			if !server.Exists(testCase.key) {
				t.Fatalf("key %q was not created", testCase.key)
			}
			if testCase.prefix != "" && server.Exists("distributed:rate:render") {
				t.Fatal("the default-prefix key exists under a custom prefix")
			}
		})
	}
}

func TestRateLimiterNamesAreIndependent(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	freezeRedisTime(t, server)
	ctx := context.Background()

	first, err := limiter.Reserve(ctx, "one", 1, time.Second)
	if err != nil {
		t.Fatalf("Reserve(one): %v", err)
	}
	second, err := limiter.Reserve(ctx, "two", 1, time.Second)
	if err != nil {
		t.Fatalf("Reserve(two): %v", err)
	}
	if first != 0 || second != 0 {
		t.Fatalf("first reservations of two names waited %s and %s, want immediate slots", first, second)
	}
	third, err := limiter.Reserve(ctx, "one", 1, time.Second)
	if err != nil {
		t.Fatalf("Reserve(one) again: %v", err)
	}
	if !waitWithin(third, time.Second, 2*time.Millisecond) {
		t.Fatalf("the second reservation of name one waited %s, want about 1s", third)
	}
	for _, key := range []string{"distributed:rate:one", "distributed:rate:two"} {
		if !server.Exists(key) {
			t.Fatalf("key %q was not created", key)
		}
	}
}

func TestRateLimiterRecoversUnreadableState(t *testing.T) {
	for name, seed := range map[string]func(server *miniredis.Miniredis){
		"foreign redis type": func(server *miniredis.Miniredis) {
			server.HSet("distributed:rate:render", "field", "value")
		},
		"unreadable string value": func(server *miniredis.Miniredis) {
			_ = server.Set("distributed:rate:render", "not a number")
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := newTestClient(t, server)
			limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
			freezeRedisTime(t, server)
			seed(server)

			wait, err := limiter.Reserve(context.Background(), "render", 1, time.Second)
			if err != nil {
				t.Fatalf("Reserve() over unreadable state: %v", err)
			}
			if wait != 0 {
				t.Fatalf("Reserve() over unreadable state waited %s, want the reset bucket's first slot", wait)
			}
			stored, err := server.Get("distributed:rate:render")
			if err != nil {
				t.Fatalf("reading the recovered key: %v", err)
			}
			if _, err := strconv.ParseFloat(stored, 64); err != nil {
				t.Fatalf("recovered key holds %q, want a theoretical arrival time", stored)
			}
		})
	}
}

func TestRateLimiterRejectsInvalidParameters(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	ctx := context.Background()

	for name, testCase := range map[string]struct {
		rateLimiterName string
		limit           int
		per             time.Duration
		want            error
	}{
		"empty name":            {rateLimiterName: "", limit: 1, per: time.Second, want: ErrInvalidName},
		"leading space in name": {rateLimiterName: " work", limit: 1, per: time.Second, want: ErrInvalidName},
		"trailing space in name": {
			rateLimiterName: "work ", limit: 1, per: time.Second, want: ErrInvalidName,
		},
		"zero limit": {rateLimiterName: "work", limit: 0, per: time.Second, want: ErrInvalidOption},
		"negative limit": {
			rateLimiterName: "work", limit: -1, per: time.Second, want: ErrInvalidOption,
		},
		"zero per": {rateLimiterName: "work", limit: 1, per: 0, want: ErrInvalidOption},
		"negative per": {
			rateLimiterName: "work", limit: 1, per: -time.Nanosecond, want: ErrInvalidOption,
		},
	} {
		t.Run(name, func(t *testing.T) {
			commands := server.CommandCount()
			if _, err := limiter.Reserve(ctx, testCase.rateLimiterName, testCase.limit, testCase.per); !errors.Is(err, testCase.want) {
				t.Fatalf("Reserve() error = %v, want %v", err, testCase.want)
			}
			if err := limiter.Wait(ctx, testCase.rateLimiterName, testCase.limit, testCase.per); !errors.Is(err, testCase.want) {
				t.Fatalf("Wait() error = %v, want %v", err, testCase.want)
			}
			if _, err := limiter.Allow(ctx, testCase.rateLimiterName, testCase.limit, testCase.per); !errors.Is(err, testCase.want) {
				t.Fatalf("Allow() error = %v, want %v", err, testCase.want)
			}
			if after := server.CommandCount(); after != commands {
				t.Fatalf("invalid calls issued %d Redis commands", after-commands)
			}
		})
	}
	if names := client.Keys(ctx, "distributed:rate:*").Val(); len(names) != 0 {
		t.Fatalf("invalid calls created keys %v", names)
	}
}

func TestRateLimiterHonorsAnAlreadyCancelledContext(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	limiter := newTestRateLimiter(t, client, RateLimiterOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := limiter.Reserve(ctx, "render", 1, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reserve() with a cancelled context = %v, want context.Canceled", err)
	}
	if err := limiter.Wait(ctx, "render", 1, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := limiter.Allow(ctx, "render", 1, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Allow() with a cancelled context = %v, want context.Canceled", err)
	}
	if commands := server.CommandCount(); commands != 0 {
		t.Fatalf("cancelled calls issued %d Redis commands", commands)
	}
}

func TestNewRateLimiterRejectsInvalidConfiguration(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	if _, err := NewRateLimiter(nil, RateLimiterOptions{}); err == nil {
		t.Fatal("NewRateLimiter(nil) succeeded, want an error")
	}
	if _, err := NewRateLimiter(client, RateLimiterOptions{Prefix: "Has Space"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("NewRateLimiter(malformed prefix) = %v, want ErrInvalidOption", err)
	}
}
