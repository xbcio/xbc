package asynq

import (
	"context"
	"errors"
	"fmt"
	"time"

	hibiken "github.com/hibiken/asynq"

	xbclog "github.com/xbcio/xbc/log"
)

// RetryDelayer is implemented by errors that carry their own redelivery delay.
// The worker's RetryDelayFunc recognizes it with errors.As, so an error that
// wraps a RetryAfter still reports the embedded delay.
//
// A self-reported delay is a redelivery time, never a retry budget: the failed
// attempt still counts against the task's max_retries, exactly as any other
// handler failure does. Implementations that keep deferring a task must
// therefore bound their own deferrals; the queue library archives the task once
// the configured budget is exhausted.
type RetryDelayer interface {
	RetryDelay() time.Duration
}

// errDeferred is the error RetryAfter substitutes for a nil cause. Returning
// nil from a handler means success, so a deferral without a cause still needs an
// error to travel with.
var errDeferred = errors.New("asynq: task deferred")

// retryAfterError carries a delay through error wrapping.
type retryAfterError struct {
	err   error
	delay time.Duration
}

func (e *retryAfterError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", e.err, e.delay)
}

// Unwrap keeps the cause reachable through errors.Is and errors.As.
func (e *retryAfterError) Unwrap() error { return e.err }

// RetryDelay implements RetryDelayer.
func (e *retryAfterError) RetryDelay() time.Duration { return e.delay }

// RetryAfter returns err marked to be redelivered after delay instead of the
// worker's exponential backoff. The wrapper keeps err reachable through
// errors.Is and errors.As, and the delay is recognized after further wrapping.
//
// A nil err is replaced with a deferral error, because a nil handler result
// means success. A negative delay is treated as zero, an immediate redelivery.
func RetryAfter(err error, delay time.Duration) error {
	if err == nil {
		err = errDeferred
	}
	if delay < 0 {
		delay = 0
	}
	return &retryAfterError{err: err, delay: delay}
}

// retryDelay reports the delay err carries, if any.
func retryDelay(err error) (time.Duration, bool) {
	var delayer RetryDelayer
	if errors.As(err, &delayer) {
		return delayer.RetryDelay(), true
	}
	return 0, false
}

// newRetryDelayFunc builds the worker's redelivery policy: an error's own
// RetryDelay wins, and every other failure backs off exponentially from base,
// doubling per attempt until cap.
func newRetryDelayFunc(base, cap time.Duration) hibiken.RetryDelayFunc {
	return func(attempt int, err error, _ *hibiken.Task) time.Duration {
		if delay, ok := retryDelay(err); ok {
			return delay
		}
		return exponentialBackoff(attempt, base, cap)
	}
}

// exponentialBackoff returns base * 2^attempt capped at cap. The loop stops as
// soon as the cap is reached, so an attempt count far beyond the point where
// the cap applies cannot overflow the duration.
func exponentialBackoff(attempt int, base, cap time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if cap <= 0 {
		cap = 60 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}
	delay := base
	for step := 0; step < attempt; step++ {
		if delay >= cap {
			return cap
		}
		delay *= 2
		if delay <= 0 {
			return cap
		}
	}
	if delay > cap {
		return cap
	}
	return delay
}

// newTaskErrorHandler logs the failures worth telling an operator about at the
// level their meaning deserves: a task deferred because its type is at the
// cluster-wide limit is routine capacity behavior rather than a fault and is
// logged at WARN, and a self-reported deferral at INFO. Every other handler
// error is left to the queue library, which already reports retry exhaustion
// and panics.
func newTaskErrorHandler(logger xbclog.Logger) hibiken.ErrorHandler {
	if logger == nil {
		logger = xbclog.Nop()
	}
	return hibiken.ErrorHandlerFunc(func(_ context.Context, task *hibiken.Task, err error) {
		switch {
		case errors.Is(err, ErrClusterConcurrencyLimit):
			logger.Warn("asynq: task deferred: cluster concurrency limit reached", "task_type", task.Type(), "error", err)
		default:
			if _, ok := retryDelay(err); ok {
				logger.Info("asynq: task deferred", "task_type", task.Type(), "error", err)
			}
		}
	})
}
