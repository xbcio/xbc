package oncetoken

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// newTestClient returns a client whose failures surface quickly: a closed
// miniredis must fail a call rather than consume the client's default retry and
// timeout budget.
func newTestClient(t *testing.T, server *miniredis.Miniredis) *goredis.Client {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{
		Addr:               server.Addr(),
		DialTimeout:        500 * time.Millisecond,
		ReadTimeout:        500 * time.Millisecond,
		WriteTimeout:       500 * time.Millisecond,
		MaxRetries:         -1,
		DialerRetries:      1,
		DialerRetryTimeout: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newTestStore starts a miniredis server and opens a Store over it with cfg's
// defaults filled in.
func newTestStore(t *testing.T, cfg Config) (*Store, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	store, err := New(newTestClient(t, server), cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store, server
}

func TestNewRejectsUnusableConstruction(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("New(nil) = nil error, want an error")
	}
	var typedNil *goredis.Client
	if _, err := New(typedNil, Config{}); err == nil {
		t.Fatal("New(typed nil) = nil error, want an error")
	}
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	if _, err := New(client, Config{DefaultTTL: -time.Second}); err == nil {
		t.Fatal("New(negative TTL) = nil error, want an error")
	}

	store, err := New(client, Config{})
	if err != nil {
		t.Fatalf("New(defaults) error = %v", err)
	}
	if store.prefix != DefaultPrefix || store.defaultTTL != DefaultTTL {
		t.Fatalf("defaults = %q, %v; want %q, %v", store.prefix, store.defaultTTL, DefaultPrefix, DefaultTTL)
	}
}

func TestIssueProducesOpaqueTokens(t *testing.T) {
	store, server := newTestStore(t, Config{})
	ctx := context.Background()

	first, err := store.Issue(ctx, "/orders/1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if !tokenPattern.MatchString(first) {
		t.Fatalf("Issue() token = %q, want 32 lowercase hexadecimal characters", first)
	}
	second, err := store.Issue(ctx, "/orders/1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if first == second {
		t.Fatal("Issue() returned the same token twice")
	}

	keys := server.Keys()
	if len(keys) != 2 {
		t.Fatalf("stored keys = %v, want two", keys)
	}
	stored := map[string]bool{DefaultPrefix + first: false, DefaultPrefix + second: false}
	for _, key := range keys {
		if _, ok := stored[key]; !ok {
			t.Fatalf("stored key %q is not prefixed with %q", key, DefaultPrefix)
		}
		stored[key] = true
	}
	for key, seen := range stored {
		if !seen {
			t.Fatalf("issued token was not stored under %q", key)
		}
	}
}

func TestConsumeRoundTripThenExpired(t *testing.T) {
	store, _ := newTestStore(t, Config{})
	ctx := context.Background()

	token, err := store.Issue(ctx, "/orders/42")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	bound, err := store.Consume(ctx, token, "/orders/42")
	if err != nil || bound != "/orders/42" {
		t.Fatalf("Consume() = %q, %v; want %q, nil", bound, err, "/orders/42")
	}
	if _, err := store.Consume(ctx, token, "/orders/42"); !errors.Is(err, ErrExpired) {
		t.Fatalf("second Consume() error = %v, want ErrExpired", err)
	}
}

func TestConsumeReportsExpiryAfterTTL(t *testing.T) {
	store, server := newTestStore(t, Config{DefaultTTL: time.Minute})
	ctx := context.Background()

	token, err := store.Issue(ctx, "/orders/42")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	server.FastForward(time.Minute + time.Second)
	if _, err := store.Consume(ctx, token, "/orders/42"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Consume() after expiry error = %v, want ErrExpired", err)
	}
}

func TestIssueTTLOverrideBeatsDefault(t *testing.T) {
	store, server := newTestStore(t, Config{DefaultTTL: time.Hour})
	ctx := context.Background()

	short, err := store.Issue(ctx, "/short", 2*time.Second)
	if err != nil {
		t.Fatalf("Issue(short TTL) error = %v", err)
	}
	fallback, err := store.Issue(ctx, "/fallback")
	if err != nil {
		t.Fatalf("Issue(default TTL) error = %v", err)
	}
	zeroOverride, err := store.Issue(ctx, "/fallback-zero", 0)
	if err != nil {
		t.Fatalf("Issue(zero TTL) error = %v", err)
	}

	server.FastForward(3 * time.Second)
	if _, err := store.Consume(ctx, short, "/short"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Consume(short TTL) error = %v, want ErrExpired", err)
	}
	for _, testCase := range []struct{ token, path string }{
		{fallback, "/fallback"},
		{zeroOverride, "/fallback-zero"},
	} {
		bound, err := store.Consume(ctx, testCase.token, testCase.path)
		if err != nil || bound != testCase.path {
			t.Fatalf("Consume(%q) = %q, %v; want %q, nil", testCase.path, bound, err, testCase.path)
		}
	}
}

func TestPathMismatchDestroysToken(t *testing.T) {
	store, _ := newTestStore(t, Config{})
	ctx := context.Background()

	token, err := store.Issue(ctx, "/allowed")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := store.Consume(ctx, token, "/denied"); !errors.Is(err, ErrPathMismatch) {
		t.Fatalf("Consume(wrong path) error = %v, want ErrPathMismatch", err)
	}
	if _, err := store.Consume(ctx, token, "/allowed"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Consume(correct path after mismatch) error = %v, want ErrExpired", err)
	}
}

func TestEmptyPathIsUnbound(t *testing.T) {
	store, _ := newTestStore(t, Config{})
	ctx := context.Background()

	unbound, err := store.Issue(ctx, "")
	if err != nil {
		t.Fatalf("Issue(empty path) error = %v", err)
	}
	if bound, err := store.Consume(ctx, unbound, "/anywhere"); err != nil || bound != "" {
		t.Fatalf("Consume(unbound token) = %q, %v; want \"\", nil", bound, err)
	}

	boundToken, err := store.Issue(ctx, "/orders/7")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if bound, err := store.Consume(ctx, boundToken, ""); err != nil || bound != "/orders/7" {
		t.Fatalf("Consume(bound token with empty path) = %q, %v; want %q, nil", bound, err, "/orders/7")
	}
}

func TestInvalidTokensAreRejectedWithoutRedisAccess(t *testing.T) {
	store, server := newTestStore(t, Config{})
	ctx := context.Background()

	if _, err := store.Issue(ctx, "/orders/1"); err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	invalid := []string{
		"",
		" ",
		"0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef0123456789abcde",
		"0123456789abcdef0123456789abcdef0",
		"0123456789abcdef0123456789abcde:",
		"0123456789abcdef0123456789abcde*",
		"0123456789abcdef0123456789abcde/",
		DefaultPrefix + "0123456789abcdef0123456789abcdef",
	}
	before := server.CommandCount()
	for _, token := range invalid {
		if _, err := store.Consume(ctx, token, "/orders/1"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Consume(%q) error = %v, want ErrInvalid", token, err)
		}
	}
	if after := server.CommandCount(); after != before {
		t.Fatalf("invalid tokens reached Redis: commands before = %d, after = %d", before, after)
	}
}

func TestPrefixIsolatesStores(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)
	first, err := New(client, Config{Prefix: "first:"})
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	second, err := New(client, Config{Prefix: "second:"})
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}
	ctx := context.Background()

	token, err := first.Issue(ctx, "/orders/1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := second.Consume(ctx, token, "/orders/1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Consume() through another prefix error = %v, want ErrExpired", err)
	}
	if bound, err := first.Consume(ctx, token, "/orders/1"); err != nil || bound != "/orders/1" {
		t.Fatalf("Consume() through the issuing prefix = %q, %v; want %q, nil", bound, err, "/orders/1")
	}
}

func TestConcurrentConsumeAdmitsExactlyOne(t *testing.T) {
	store, _ := newTestStore(t, Config{})
	ctx := context.Background()

	token, err := store.Issue(ctx, "/race")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	const consumers = 64
	var (
		waitGroup sync.WaitGroup
		start     = make(chan struct{})
		succeeded atomic.Int64
		expired   atomic.Int64
		otherMu   sync.Mutex
		otherErr  error
	)
	waitGroup.Add(consumers)
	for range consumers {
		go func() {
			defer waitGroup.Done()
			<-start
			bound, err := store.Consume(ctx, token, "/race")
			switch {
			case err == nil && bound == "/race":
				succeeded.Add(1)
			case errors.Is(err, ErrExpired):
				expired.Add(1)
			default:
				otherMu.Lock()
				if otherErr == nil {
					otherErr = fmt.Errorf("unexpected outcome: path %q, error %v", bound, err)
				}
				otherMu.Unlock()
			}
		}()
	}
	close(start)
	waitGroup.Wait()
	if otherErr != nil {
		t.Fatal(otherErr)
	}
	if got := succeeded.Load(); got != 1 {
		t.Fatalf("successful consumers = %d, want 1", got)
	}
	if got := expired.Load(); got != consumers-1 {
		t.Fatalf("ErrExpired consumers = %d, want %d", got, consumers-1)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	store, server := newTestStore(t, Config{})
	ctx := context.Background()

	mismatched, err := store.Issue(ctx, "/allowed")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	mismatchErr := func() error {
		_, err := store.Consume(ctx, mismatched, "/denied")
		return err
	}()
	if !errors.Is(mismatchErr, ErrPathMismatch) {
		t.Fatalf("Consume(wrong path) error = %v, want ErrPathMismatch", mismatchErr)
	}
	if strings.Contains(mismatchErr.Error(), mismatched) {
		t.Fatalf("mismatch error %q repeats the token", mismatchErr)
	}
	expiredErr := func() error {
		_, err := store.Consume(ctx, mismatched, "/allowed")
		return err
	}()
	if !errors.Is(expiredErr, ErrExpired) {
		t.Fatalf("Consume(consumed token) error = %v, want ErrExpired", expiredErr)
	}
	if strings.Contains(expiredErr.Error(), mismatched) {
		t.Fatalf("expired error %q repeats the token", expiredErr)
	}

	networkToken, err := store.Issue(ctx, "/allowed")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	server.Close()
	_, networkErr := store.Consume(ctx, networkToken, "/allowed")
	if networkErr == nil {
		t.Fatal("Consume() against a closed server = nil error, want an error")
	}
	if errors.Is(networkErr, ErrInvalid) || errors.Is(networkErr, ErrExpired) || errors.Is(networkErr, ErrPathMismatch) {
		t.Fatalf("a Redis failure was reported as a token verdict: %v", networkErr)
	}
	if strings.Contains(networkErr.Error(), networkToken) {
		t.Fatalf("transport error %q repeats the token", networkErr)
	}
}
