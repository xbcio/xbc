package async

import "context"

// executor is the unexported seam between Pool's accounting (concurrency
// limits, queueing, and bookkeeping all live in Pool, never here) and the
// strategy that actually runs a task. goroutineExecutor is the only
// implementation in this step; a later step adds an ants-backed executor
// behind the same seam without changing Pool.
type executor interface {
	// Go runs fn. A goroutine implementation never returns a non-nil error;
	// a bounded-worker implementation (ants, in a later step) may return one
	// when it cannot accept fn at all.
	Go(fn func()) error
	// Release stops accepting new work and waits, within ctx, for the
	// executor's own resources to wind down. A goroutine implementation has
	// no pooled resource and returns immediately.
	Release(ctx context.Context) error
}

// goroutineExecutor runs every task on its own goroutine. It owns no pooled
// resource, so Release is a no-op.
type goroutineExecutor struct{}

func newGoroutineExecutor() *goroutineExecutor { return &goroutineExecutor{} }

func (*goroutineExecutor) Go(fn func()) error {
	go fn()
	return nil
}

func (*goroutineExecutor) Release(context.Context) error { return nil }
