package asynq

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	xbclog "github.com/xbcio/xbc/log"
)

const (
	// semaphoreKeyPrefix namespaces the cluster-wide task-type slots. The task
	// type follows the prefix unchanged; the full layout is documented in
	// doc.go.
	semaphoreKeyPrefix = "xbc:asynq:sem:"

	// semaphoreOpTimeout bounds one Redis operation of the semaphore. It is
	// independent of the task's own context because releasing a slot must not
	// be skipped just because the handler's context expired.
	semaphoreOpTimeout = 5 * time.Second

	// semaphoreRenewFailureLimit is how many consecutive Redis errors the
	// renewal loop tolerates before treating the lease as lost. A single failed
	// renewal does not mean the slot is gone, but repeated failures mean the
	// token can no longer be kept alive and the handler would soon be running
	// without the guarantee it was admitted under.
	semaphoreRenewFailureLimit = 3

	// semaphoreMinRenewInterval keeps a very short lease from turning the
	// renewal loop into a busy loop.
	semaphoreMinRenewInterval = 10 * time.Millisecond
)

// tryAcquireScript adds a lease token if fewer than limit live tokens exist.
// Expired tokens are discarded before counting, and a key holding an
// unexpected Redis type is dropped first: a key claimed by an unrelated writer
// must not wedge the task type forever.
var tryAcquireScript = goredis.NewScript(`
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
var renewScript = goredis.NewScript(`
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
var releaseScript = goredis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'zset' then
    return 0
end
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

// cleanupExpiredScript removes expired tokens and leaves live ones alone, so a
// reset is safe during a rolling restart. A key of another Redis type is
// dropped, matching the acquisition path's recovery.
var cleanupExpiredScript = goredis.NewScript(`
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

// clusterLimit is the resolved deployment cap of one task type: how many
// handlers may run across the cluster, the lease one slot is held under, and
// the delay a deferred delivery reports.
type clusterLimit struct {
	limit      int
	lease      time.Duration
	retryDelay time.Duration
}

// clusterSemaphore owns the Redis operations behind the cluster-wide cap.
// Every worker of one process shares one value, so a process with several
// workload workers still counts its own executions against the same slots.
type clusterSemaphore struct {
	client goredis.UniversalClient
	logger xbclog.Logger
}

func newClusterSemaphore(client goredis.UniversalClient, logger xbclog.Logger) *clusterSemaphore {
	if logger == nil {
		logger = xbclog.Nop()
	}
	return &clusterSemaphore{client: client, logger: logger}
}

// clusterSemaphoreKey is the Redis key holding one task type's slots.
func clusterSemaphoreKey(taskType string) string {
	return semaphoreKeyPrefix + taskType
}

// run acquires one slot through tryAcquire and holds it for the duration of
// handler. The handler's context is cancelled when the lease can no longer be
// kept alive: at that point the slot may already belong to another execution,
// so the work it was admitted for must stop rather than run unprotected.
func (s *clusterSemaphore) run(ctx context.Context, taskType string, limit clusterLimit, handler func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(context.Background(), semaphoreOpTimeout)
	token, acquired, err := s.tryAcquire(opCtx, taskType, limit.limit, limit.lease)
	cancel()
	if err != nil {
		return fmt.Errorf("asynq: acquire cluster semaphore for task type %q: %w", taskType, err)
	}
	if !acquired {
		return RetryAfter(
			fmt.Errorf("%w: task type %q is at its cluster-wide limit of %d", ErrClusterConcurrencyLimit, taskType, limit.limit),
			limit.retryDelay,
		)
	}
	// The delivery may have been cancelled while the acquisition was in
	// flight. Hand the slot back before reporting the cancellation, so the
	// abandoned attempt does not hold capacity until its lease lapses.
	if err := ctx.Err(); err != nil {
		s.releaseSlot(taskType, token)
		return err
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	stopRenewal := s.startRenewal(taskType, token, limit.lease, cancelRun)
	defer func() {
		stopRenewal()
		cancelRun()
		s.releaseSlot(taskType, token)
	}()
	return handler(runCtx)
}

// tryAcquire adds one token when the live-token count is below limit. The
// returned token identifies the slot for renewal and release.
func (s *clusterSemaphore) tryAcquire(ctx context.Context, taskType string, limit int, lease time.Duration) (string, bool, error) {
	token := newClusterSemaphoreToken(taskType)
	acquired, err := tryAcquireScript.Run(ctx, s.client,
		[]string{clusterSemaphoreKey(taskType)},
		token, limit, nowMillis(), leaseMillis(lease),
	).Int()
	if err != nil {
		return "", false, err
	}
	return token, acquired == 1, nil
}

// renew extends the token's lease. It reports false once the token is no
// longer present, which is the signal that this execution lost its slot.
func (s *clusterSemaphore) renew(ctx context.Context, taskType, token string, lease time.Duration) (bool, error) {
	renewed, err := renewScript.Run(ctx, s.client,
		[]string{clusterSemaphoreKey(taskType)},
		token, nowMillis(), leaseMillis(lease),
	).Int()
	if err != nil {
		return false, err
	}
	return renewed == 1, nil
}

// release removes the token. It is a no-op when the key is gone or holds
// another Redis type.
func (s *clusterSemaphore) release(ctx context.Context, taskType, token string) error {
	return releaseScript.Run(ctx, s.client, []string{clusterSemaphoreKey(taskType)}, token).Err()
}

// reset removes expired tokens and leaves live ones alone. It reports how many
// live slots remain.
func (s *clusterSemaphore) reset(ctx context.Context, taskType string) (int, error) {
	remaining, err := cleanupExpiredScript.Run(ctx, s.client, []string{clusterSemaphoreKey(taskType)}, nowMillis()).Int()
	if err != nil {
		return 0, err
	}
	return remaining, nil
}

// releaseSlot releases with a context of its own: the handler's context is
// often already cancelled when the slot is handed back.
func (s *clusterSemaphore) releaseSlot(taskType, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), semaphoreOpTimeout)
	defer cancel()
	if err := s.release(ctx, taskType, token); err != nil {
		s.logger.Error("asynq: release cluster semaphore", "task_type", taskType, "error", err)
	}
}

// startRenewal extends the lease every lease/3 until the returned stop
// function is called. When renewal observes that the token is gone, or that
// Redis could not renew it for semaphoreRenewFailureLimit consecutive
// attempts, it calls onLost and stops: the slot can no longer be guaranteed.
func (s *clusterSemaphore) startRenewal(taskType, token string, lease time.Duration, onLost func()) func() {
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
				renewed, err := s.renew(renewCtx, taskType, token, lease)
				renewCancel()
				if err != nil {
					failures++
					s.logger.Error("asynq: renew cluster semaphore", "task_type", taskType, "error", err)
					if failures >= semaphoreRenewFailureLimit {
						s.logger.Error("asynq: cluster semaphore lease could not be renewed", "task_type", taskType, "failures", failures)
						onLost()
						return
					}
					continue
				}
				failures = 0
				if !renewed {
					s.logger.Warn("asynq: cluster semaphore slot disappeared before the task finished", "task_type", taskType)
					onLost()
					return
				}
			}
		}
	}()
	return cancel
}

// newClusterSemaphoreToken identifies one execution's slot. The process's
// sequence number separates executions within one process and the timestamp
// separates processes, which is enough for a member that only needs to be
// unique, not unpredictable.
func newClusterSemaphoreToken(taskType string) string {
	sequence := clusterSemaphoreTokenSeq.Add(1)
	return fmt.Sprintf("%s:%d:%d", taskType, time.Now().UnixNano(), sequence)
}

var clusterSemaphoreTokenSeq atomic.Uint64

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
