package saml

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestVerifyRejectsReplayedAssertion(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, nil)
	mustVerify(t, provider, responseInput(response))
	mustRejectResponse(t, provider, responseInput(response), ErrReplay)
}

func TestVerifyAcceptsDistinctAssertions(t *testing.T) {
	provider := newTestProvider(t, nil)
	first := buildSignedResponse(t, nil)
	second := buildSignedResponse(t, func(options *responseOptions) {
		options.assertionID = "_assertionfedcba9876543210"
	})
	mustVerify(t, provider, responseInput(first))
	mustVerify(t, provider, responseInput(second))
}

func TestVerifyFailsClosedWhenReplayCacheFails(t *testing.T) {
	cache := &testReplayCache{err: errors.New("cache unavailable")}
	provider := newTestProvider(t, func(config *Config) {
		config.ReplayCache = cache
	})
	mustRejectResponse(t, provider, responseInput(buildSignedResponse(t, nil)), ErrUnavailable)
}

func TestVerifyReportsCacheReplay(t *testing.T) {
	cache := &testReplayCache{}
	provider := newTestProvider(t, func(config *Config) {
		config.ReplayCache = cache
	})
	response := buildSignedResponse(t, nil)
	mustVerify(t, provider, responseInput(response))
	mustRejectResponse(t, provider, responseInput(response), ErrReplay)
}

func TestVerifyDoesNotClaimReplayForInvalidResponse(t *testing.T) {
	cache := &testReplayCache{}
	provider := newTestProvider(t, func(config *Config) {
		config.ReplayCache = cache
	})
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.destination = "https://sp.example.com/other"
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
	if len(cache.claims) != 0 {
		t.Fatalf("replay cache was claimed for an invalid response: %v", cache.claims)
	}
}

// recordingReplayCache records the expiry Verify passes to the cache.
type recordingReplayCache struct {
	expires time.Time
}

func (c *recordingReplayCache) Claim(_ context.Context, _ string, expires time.Time) error {
	c.expires = expires
	return nil
}

func TestVerifyClaimsUntilEndOfValidityWindow(t *testing.T) {
	cache := &recordingReplayCache{}
	provider := newTestProvider(t, func(config *Config) {
		config.ReplayCache = cache
		config.Leeway = 45 * time.Second
	})
	mustVerify(t, provider, responseInput(buildSignedResponse(t, nil)))
	// The confirmation expiry equals the conditions expiry; the claim extends
	// to the end of that window plus the configured leeway.
	want := testNow.Add(5*time.Minute + 45*time.Second)
	if !cache.expires.Equal(want) {
		t.Fatalf("replay expiry = %s, want %s", cache.expires, want)
	}
}

func TestVerifyClaimsEarlierConfirmationExpiry(t *testing.T) {
	cache := &recordingReplayCache{}
	provider := newTestProvider(t, func(config *Config) {
		config.ReplayCache = cache
	})
	confirmationExpiry := testNow.Add(2 * time.Minute)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.confirmationNotOnOrAfter = &confirmationExpiry
	})
	mustVerify(t, provider, responseInput(response))
	want := confirmationExpiry.Add(defaultLeeway)
	if !cache.expires.Equal(want) {
		t.Fatalf("replay expiry = %s, want %s", cache.expires, want)
	}
}

func TestVerifyConcurrentReplayIsClaimedOnce(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, nil)
	const attempts = 16
	var wait sync.WaitGroup
	results := make(chan error, attempts)
	for index := 0; index < attempts; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := Verify(t.Context(), provider, responseInput(response))
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	replays := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrReplay):
			replays++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || replays != attempts-1 {
		t.Fatalf("successes = %d, replays = %d, want 1 and %d", successes, replays, attempts-1)
	}
}

func TestMemoryReplayCacheRejectsDuplicateUntilExpiry(t *testing.T) {
	now := testNow
	cache := newMemoryReplayCache(func() time.Time { return now })
	expires := now.Add(time.Minute)
	if err := cache.Claim(t.Context(), "_id", expires); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := cache.Claim(t.Context(), "_id", expires); !errors.Is(err, ErrReplay) {
		t.Fatalf("second claim = %v, want ErrReplay", err)
	}
	now = expires.Add(-time.Nanosecond)
	if err := cache.Claim(t.Context(), "_id", expires); !errors.Is(err, ErrReplay) {
		t.Fatalf("claim before expiry = %v, want ErrReplay", err)
	}
	now = expires
	if err := cache.Claim(t.Context(), "_id", expires); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
}

func TestMemoryReplayCacheRejectsInvalidClaims(t *testing.T) {
	cache := NewMemoryReplayCache()
	if err := cache.Claim(nil, "_id", testNow); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Claim(nil context) = %v, want ErrNilContext", err)
	}
	if err := cache.Claim(t.Context(), "", testNow); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Claim(empty id) = %v, want ErrUnavailable", err)
	}
	if err := cache.Claim(t.Context(), "_id", time.Time{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Claim(zero expiry) = %v, want ErrUnavailable", err)
	}
	var nilCache *MemoryReplayCache
	if err := nilCache.Claim(t.Context(), "_id", testNow); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil cache Claim = %v, want ErrUnavailable", err)
	}
}

func TestMemoryReplayCacheReturnsErrUnavailableForDoneContext(t *testing.T) {
	cache := NewMemoryReplayCache()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cache.Claim(cancelled, "_id", testNow.Add(time.Minute)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Claim(cancelled context) = %v, want ErrUnavailable", err)
	}
}
