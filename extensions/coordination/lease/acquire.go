package lease

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"time"
)

// defaultRetryInterval is the wait between attempts when none is configured.
// It matches the retry delay of the redsync library whose blocking acquisition
// Acquire replaces.
const defaultRetryInterval = 50 * time.Millisecond

// ErrNotAcquired is returned by Acquire when it has used its attempt budget
// without winning the key while the caller's context was still live. It is
// distinct from a context error so a caller can tell its own budget from a
// deadline and retry later or treat the budget as the policy it configured.
var ErrNotAcquired = errors.New("lease: not acquired")

// AcquireOption configures Acquire.
type AcquireOption func(*acquireOptions) error

type acquireOptions struct {
	interval    time.Duration
	maxAttempts int
}

// WithRetryInterval sets the nominal wait between attempts. Each wait is drawn
// from the upper half of the interval, [interval/2, interval], so processes
// started together do not poll the store in lockstep; no wait exceeds the
// configured interval. The default is 50ms, the retry delay of the redsync
// library whose blocking acquisition Acquire replaces.
func WithRetryInterval(interval time.Duration) AcquireOption {
	return func(options *acquireOptions) error {
		if interval < time.Millisecond {
			return fmt.Errorf("lease: retry interval must be at least 1ms, got %s", interval)
		}
		options.interval = interval
		return nil
	}
}

// WithMaxAttempts caps the number of TryAcquire calls, the first included.
// Zero, the default, removes the cap and leaves the caller's context as the
// only bound. The budget corresponds to redsync's tries setting; redsync's
// default of 32 attempts is expressed as WithMaxAttempts(32).
func WithMaxAttempts(attempts int) AcquireOption {
	return func(options *acquireOptions) error {
		if attempts < 0 {
			return fmt.Errorf("lease: maximum attempts must not be negative, got %d", attempts)
		}
		options.maxAttempts = attempts
		return nil
	}
}

// Acquire blocks until locker grants key or ctx ends, polling on a jittered
// retry interval. It is a function over Locker rather than a method of a new
// interface, so every backend that implements TryAcquire gains blocking
// acquisition without implementing anything more.
//
// Contention is the only outcome it retries: a TryAcquire error is returned
// immediately, wrapped, as the redsync call it replaces does. An acquisition
// that committed remotely just as ctx ended is returned as a lease with a nil
// error, because a lease won is a lease that must be released; callers observe
// cancellation through a failed attempt, not through a lease.
//
// When ctx ends before a lease is won, the returned error wraps ctx.Err(), so
// errors.Is(err, context.Canceled) and errors.Is(err, context.DeadlineExceeded)
// classify it. When the WithMaxAttempts budget is exhausted while ctx is still
// live, the returned error wraps ErrNotAcquired. A locker that reports
// acquired=true with a nil lease is a contract violation and is reported as an
// error rather than returned as a usable lease.
func Acquire(ctx context.Context, locker Locker, key, claimant string, ttl time.Duration, options ...AcquireOption) (Lease, error) {
	if locker == nil {
		return nil, errors.New("lease: Acquire requires a non-nil Locker")
	}
	config := acquireOptions{interval: defaultRetryInterval}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("lease: acquire option %d is nil", index)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("lease: acquire %q: %w", key, err)
		}
		held, acquired, err := locker.TryAcquire(ctx, key, claimant, ttl)
		if err != nil {
			return nil, fmt.Errorf("lease: acquire %q: %w", key, err)
		}
		if acquired {
			if held == nil {
				return nil, fmt.Errorf("lease: acquire %q: locker reported the key acquired with a nil lease", key)
			}
			return held, nil
		}
		if config.maxAttempts > 0 && attempt >= config.maxAttempts {
			return nil, fmt.Errorf("%w: %q after %d attempts", ErrNotAcquired, key, attempt)
		}
		if err := waitRetry(ctx, retryDelay(config.interval)); err != nil {
			return nil, fmt.Errorf("lease: acquire %q: %w", key, err)
		}
	}
}

// waitRetry sleeps for delay unless ctx ends first, reporting the context's
// error when it does.
func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelay draws the wait before the next attempt from the upper half of
// interval, [interval/2, interval]. The spread breaks the lockstep of callers
// started together -- a rolling restart, a node that came back with its pods --
// and the form keeps every wait bounded by the configured interval.
func retryDelay(interval time.Duration) time.Duration {
	half := interval / 2
	if half <= 0 {
		return interval
	}
	return half + time.Duration(mathrand.Int64N(int64(half)+1))
}
