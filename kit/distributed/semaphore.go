package distributed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// semaphoreOpTimeout bounds one Redis operation of the semaphore. It is
	// independent of the caller's context because releasing a slot must not be
	// skipped just because the context it was acquired under expired.
	semaphoreOpTimeout = 5 * time.Second

	// semaphoreRenewFailureLimit is how many consecutive Redis errors the
	// renewal loop tolerates before treating the lease as lost. A single failed
	// renewal does not mean the slot is gone, but repeated failures mean the
	// token can no longer be kept alive and the work would soon run without the
	// guarantee it was admitted under.
	semaphoreRenewFailureLimit = 3

	// semaphoreMinRenewInterval keeps a very short lease from turning the
	// renewal loop into a busy loop.
	semaphoreMinRenewInterval = 10 * time.Millisecond
)

// tryAcquireScript adds a lease token if fewer than limit live tokens exist.
// Expired tokens are discarded before counting, and a key holding an
// unexpected Redis type is dropped first: a key claimed by an unrelated writer
// must not wedge the semaphore forever.
var tryAcquireScript = redis.NewScript(`
local key   = KEYS[1]
local token = ARGV[1]
local limit = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local lease = tonumber(ARGV[4])
local typ = redis.call('TYPE', key).ok
if typ ~= 'none' and typ ~= 'zset' then
    redis.call('DEL', key)
end
redis.call('ZREMRANGEBYSCORE', key, '-inf', now)
if redis.call('ZCARD', key) < limit then
    redis.call('ZADD', key, now + lease, token)
    redis.call('PEXPIRE', key, lease * 2)
    return 1
end
return 0
`)

// renewScript extends a token's lease only if the token is still present, so a
// renewal cannot resurrect a slot that expired or was reset away.
var renewScript = redis.NewScript(`
local key   = KEYS[1]
local token = ARGV[1]
local now   = tonumber(ARGV[2])
local lease = tonumber(ARGV[3])
if redis.call('TYPE', key).ok ~= 'zset' then
    return 0
end
if redis.call('ZSCORE', key, token) then
    redis.call('ZADD', key, 'XX', now + lease, token)
    redis.call('PEXPIRE', key, lease * 2)
    return 1
end
return 0
`)

// releaseScript removes only the token this execution owns.
var releaseScript = redis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'zset' then
    return 0
end
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

// cleanupExpiredScript removes expired tokens and leaves live ones alone, so a
// reset is safe while replicas are running. A key of another Redis type is
// dropped, matching the acquisition path's recovery.
var cleanupExpiredScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local typ = redis.call('TYPE', key).ok
if typ ~= 'none' and typ ~= 'zset' then
    redis.call('DEL', key)
    return 0
end
redis.call('ZREMRANGEBYSCORE', key, '-inf', now)
return redis.call('ZCARD', key)
`)

// SemaphoreOptions configures one semaphore. The zero value is valid: it uses
// the default prefix and slog.Default().
type SemaphoreOptions struct {
	// Prefix namespaces every key this semaphore uses. It defaults to
	// DefaultPrefix. Two deployments that share a Redis instance and prefix
	// share slots; give each logical cluster its own prefix. A prefix that is
	// not safe in a Redis key is rejected with ErrInvalidOption.
	Prefix string
	// Logger receives renewal failures, lost slots, and release failures. It
	// defaults to slog.Default().
	Logger *slog.Logger
}

// Semaphore bounds how many executions of one named operation run at once
// across every process sharing the Redis instance.
//
// A slot is a member of a Redis sorted set scored by its lease expiry in Unix
// milliseconds. Acquisition discards expired members before counting, a
// running execution renews its member every lease/3, and the key expires after
// twice the lease, so a process that dies frees its slots when the leases
// lapse instead of blocking the semaphore forever. A key holding a Redis type
// other than a sorted set is deleted and recreated, recovering a key claimed by
// an unrelated writer.
//
// Run holds one slot for the duration of the callback. When renewal observes
// that the member is gone -- or that Redis could not renew it for three
// consecutive attempts -- the callback's context is cancelled: the slot no
// longer protects the work that was admitted under it, so that work must stop
// rather than continue unprotected.
//
// There is no process-wide registry: two Semaphore values with the same prefix
// and name operate on the same slots, which is what makes the limit
// cluster-wide.
type Semaphore struct {
	client redis.UniversalClient
	log    *slog.Logger
	prefix string
}

// NewSemaphore returns a semaphore backed by client. Construction performs no
// I/O; Run and Reset do. A nil client is rejected.
func NewSemaphore(client redis.UniversalClient, options SemaphoreOptions) (*Semaphore, error) {
	if client == nil {
		return nil, errors.New("distributed: NewSemaphore requires a non-nil redis client")
	}
	prefix := strings.TrimSpace(options.Prefix)
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if !validMapName(prefix) {
		return nil, fmt.Errorf("%w: prefix %q must match %s", ErrInvalidOption, prefix, mapNamePattern)
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Semaphore{client: client, log: logger, prefix: prefix}, nil
}

// key is the Redis key holding one name's slots.
func (s *Semaphore) key(name string) string {
	return s.prefix + ":sem:" + name
}

// Run acquires one slot of name, holds it for the duration of fn, and returns
// fn's error. limit must be positive and is the maximum number of live slots;
// lease must be positive and is how long one slot survives without renewal.
//
// It reports ErrLimitReached when every slot is taken, ErrInvalidName when name
// is empty or has leading or trailing whitespace, and ErrInvalidOption when
// limit or lease is not positive. The limit and lease are validated per call
// because they belong to the operation, not to the semaphore value.
//
// The callback's context is cancelled when the lease can no longer be kept
// alive. The slot is released when fn returns, whatever it returned, using a
// fresh context so that releasing never depends on the callback's own context.
func (s *Semaphore) Run(ctx context.Context, name string, limit int, lease time.Duration, fn func(context.Context) error) error {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSemaphoreName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if limit <= 0 {
		return fmt.Errorf("%w: semaphore %q limit must be positive, got %d", ErrInvalidOption, name, limit)
	}
	if lease <= 0 {
		return fmt.Errorf("%w: semaphore %q lease must be positive, got %s", ErrInvalidOption, name, lease)
	}

	opCtx, cancel := context.WithTimeout(context.Background(), semaphoreOpTimeout)
	token, acquired, err := s.tryAcquire(opCtx, name, limit, lease)
	cancel()
	if err != nil {
		return fmt.Errorf("distributed: semaphore %q: acquire: %w", name, err)
	}
	if !acquired {
		return ErrLimitReached
	}
	// The caller may have been cancelled while the acquisition was in flight.
	// Hand the slot back before reporting the cancellation, so the abandoned
	// attempt does not hold capacity until its lease lapses.
	if err := ctx.Err(); err != nil {
		s.releaseSlot(name, token)
		return err
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	stopRenewal := s.startRenewal(name, token, lease, cancelRun)
	defer func() {
		stopRenewal()
		cancelRun()
		s.releaseSlot(name, token)
	}()
	return fn(runCtx)
}

// Reset removes expired slots of name and leaves live ones alone, reporting how
// many live slots remain. It is safe while replicas are running, and a name
// that holds no key succeeds without creating one. A key of another Redis type
// is dropped.
func (s *Semaphore) Reset(ctx context.Context, name string) (int, error) {
	ctx = orBackground(ctx)
	if !validSemaphoreName(name) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	remaining, err := cleanupExpiredScript.Run(ctx, s.client, []string{s.key(name)}, nowMillis()).Int()
	if err != nil {
		return 0, fmt.Errorf("distributed: semaphore %q: reset: %w", name, err)
	}
	return remaining, nil
}

// validSemaphoreName accepts any non-empty name without leading or trailing
// whitespace. A semaphore name is a caller-chosen label, so it is not held to
// the Redis-key charset a map name needs; it must merely identify something
// and must not silently differ from the name an operator wrote down.
func validSemaphoreName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name
}

// tryAcquire adds one token when the live-token count is below limit. The
// returned token identifies the slot for renewal and release.
func (s *Semaphore) tryAcquire(ctx context.Context, name string, limit int, lease time.Duration) (string, bool, error) {
	token := newSemaphoreToken(name)
	acquired, err := tryAcquireScript.Run(ctx, s.client,
		[]string{s.key(name)},
		token, limit, nowMillis(), leaseMillis(lease),
	).Int()
	if err != nil {
		return "", false, err
	}
	return token, acquired == 1, nil
}

// renew extends the token's lease. It reports false once the token is no
// longer present, which is the signal that this execution lost its slot.
func (s *Semaphore) renew(ctx context.Context, name, token string, lease time.Duration) (bool, error) {
	renewed, err := renewScript.Run(ctx, s.client,
		[]string{s.key(name)},
		token, nowMillis(), leaseMillis(lease),
	).Int()
	if err != nil {
		return false, err
	}
	return renewed == 1, nil
}

// release removes the token. It is a no-op when the key is gone or holds
// another Redis type.
func (s *Semaphore) release(ctx context.Context, name, token string) error {
	return releaseScript.Run(ctx, s.client, []string{s.key(name)}, token).Err()
}

// releaseSlot releases with a context of its own: the callback's context is
// often already cancelled when the slot is handed back.
func (s *Semaphore) releaseSlot(name, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), semaphoreOpTimeout)
	defer cancel()
	if err := s.release(ctx, name, token); err != nil {
		s.log.Error("distributed: release semaphore", "semaphore", name, "error", err)
	}
}

// startRenewal extends the lease every lease/3 until the returned stop
// function is called. When renewal observes that the token is gone, or that
// Redis could not renew it for semaphoreRenewFailureLimit consecutive
// attempts, it calls onLost and stops: the slot can no longer be guaranteed.
func (s *Semaphore) startRenewal(name, token string, lease time.Duration, onLost func()) func() {
	interval := lease / 3
	if interval < semaphoreMinRenewInterval {
		interval = semaphoreMinRenewInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(context.Background(), semaphoreOpTimeout)
				renewed, err := s.renew(renewCtx, name, token, lease)
				renewCancel()
				if err != nil {
					failures++
					s.log.Error("distributed: renew semaphore", "semaphore", name, "error", err)
					if failures >= semaphoreRenewFailureLimit {
						s.log.Error("distributed: semaphore lease could not be renewed", "semaphore", name, "failures", failures)
						onLost()
						return
					}
					continue
				}
				failures = 0
				if !renewed {
					s.log.Warn("distributed: semaphore slot disappeared before the work finished", "semaphore", name)
					onLost()
					return
				}
			}
		}
	}()
	return cancel
}

// newSemaphoreToken identifies one execution's slot. The process's sequence
// number separates executions within one process and the timestamp separates
// processes, which is enough for a member that only needs to be unique, not
// unpredictable.
func newSemaphoreToken(name string) string {
	sequence := semaphoreTokenSeq.Add(1)
	return fmt.Sprintf("%s:%d:%d", name, time.Now().UnixNano(), sequence)
}

var semaphoreTokenSeq atomic.Uint64

func nowMillis() int64 { return time.Now().UnixMilli() }

// leaseMillis converts a lease to the millisecond score resolution the sorted
// set uses. A sub-millisecond lease is rounded up so it does not become zero
// and expire immediately.
func leaseMillis(lease time.Duration) int64 {
	millis := lease.Milliseconds()
	if millis <= 0 {
		return 1
	}
	return millis
}

// ErrLimitReached reports that every slot of a semaphore is taken.
var ErrLimitReached = errors.New("distributed: semaphore limit reached")
