package async

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/panjf2000/ants/v2"

	"github.com/xbcio/xbc/log"
)

// antsExecutor runs tasks on a fixed-size, goroutine-reusing worker pool
// backed by github.com/panjf2000/ants/v2. It implements the same executor
// seam as goroutineExecutor; Pool's own semaphore (MaxConcurrency) is the
// only admission control, so this pool's size always equals MaxConcurrency.
// Go is left in ants' default blocking mode: see its own doc comment for why
// a blocked Submit here is always brief and never a deadlock risk.
//
// Go wraps every task so panic recovery and the per-task pprof label -- both
// otherwise provided by Pool.runOne for a one-task-per-goroutine executor --
// are unaffected by ants reusing the calling goroutine across tasks: without
// a label scoped to the single pprof.Do call inside the wrapper, a label set
// by one task could otherwise outlive it and be attributed to whatever task
// the same reused worker goroutine runs next.
type antsExecutor struct {
	pool *ants.Pool
}

// newAntsExecutor creates an ants pool sized to maxConcurrency. maxConcurrency
// must be positive; prepareConfig enforces that before an antsExecutor is ever
// constructed. The pool is left in ants' default blocking mode (Nonblocking
// false, MaxBlockingTasks 0, meaning unbounded blocking rather than unbounded
// queueing: see Go's own doc comment for why a blocked Submit here can only
// ever be brief).
func newAntsExecutor(maxConcurrency int, cfg AntsConfig, logger log.Logger) (*antsExecutor, error) {
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("async: ants executor requires a positive max_concurrency, got %d", maxConcurrency)
	}
	if logger == nil {
		logger = log.Nop()
	}
	options := []ants.Option{
		ants.WithExpiryDuration(cfg.ExpiryDuration),
		ants.WithPreAlloc(cfg.PreAlloc),
		ants.WithDisablePurge(cfg.DisablePurge),
		ants.WithLogger(antsLoggerAdapter{log: logger}),
		// PanicHandler is only a backstop: Go's own wrapper already recovers
		// and logs every task's panic before it can reach ants, matching
		// goroutineExecutor and keeping the stack trace and task name
		// together in one log entry. If a panic ever did reach ants despite
		// that -- defer ordering changing underneath, for instance -- this
		// still turns it into a logged event rather than a crashed worker.
		ants.WithPanicHandler(func(recovered any) {
			logger.Error("async: panic reached ants PanicHandler (backstop; expected recovery is in the task wrapper)",
				"panic", fmt.Sprintf("%v", recovered),
				"stack", string(debug.Stack()),
			)
		}),
	}
	pool, err := ants.NewPool(maxConcurrency, options...)
	if err != nil {
		return nil, fmt.Errorf("async: failed to create ants pool: %w", err)
	}
	return &antsExecutor{pool: pool}, nil
}

// Go submits fn to the ants pool, blocking briefly if every ants worker is
// currently busy. That block can only ever be momentary and can never
// deadlock, for two independent reasons that together rule out every caller
// of Go waiting on itself or on each other:
//
//  1. Go is only ever called while holding one of the pool's own
//     max_concurrency running slots (admit increments p.running before
//     Spawn calls exec.Go for a freshly admitted task; runWorker already
//     holds its slot when it loops to the next queued task). The ants pool
//     is sized to exactly max_concurrency, so whenever Go is called, at
//     least one ants worker is either idle or already in the process of
//     returning from the task that is relinquishing this exact slot (the
//     revertWorker-after-return gap described in HEAD 7465ec6): either way,
//     a worker becomes available to accept Submit without any new slot
//     being reserved, so the wait is bounded by that worker's own
//     in-progress return, not by unrelated work.
//  2. A queued task is never submitted by a new, independent Spawn call
//     from inside a running task: runWorker (the worker loop in pool.go)
//     pops and runs queued work on the same goroutine that just finished a
//     task, so the goroutine that would block in Submit is never the same
//     goroutine Submit is waiting to free. A nested async.Spawn from inside
//     a task body is a brand new Spawn call like any other -- it either
//     finds a running slot and calls Go itself (same argument as (1), since
//     some other worker, not the caller's own, must be idle or returning),
//     or it queues, in which case it is not calling Go at all right now and
//     will instead be picked up later by whichever worker's runWorker loop
//     reaches it. No worker is ever waiting on a Submit that only its own
//     eventual return could satisfy.
//
// Any error Submit still returns -- ErrPoolOverload or otherwise -- is
// therefore an actual invariant violation (Pool's semaphore and ants' own
// worker count disagreeing about available capacity for a reason other than
// the bounded revert gap above) rather than a saturation signal, so it is
// returned (and logged by the caller) rather than translated into
// ErrSaturated.
func (e *antsExecutor) Go(fn func()) error {
	if err := e.pool.Submit(fn); err != nil {
		if errors.Is(err, ants.ErrPoolOverload) {
			return fmt.Errorf("async: ants pool rejected a task its own size should have admitted: %w", err)
		}
		return err
	}
	return nil
}

// Release waits, within ctx, for every running ants worker to return, then
// releases the pool's own goroutines. It is called from Pool.stop, after
// Pool's own wait for inFlight has already completed or ctx has expired;
// Release's own wait is bounded by whatever time remains on ctx (or runs
// unbounded if ctx carries no deadline), and ReleaseContext's error, if any,
// is returned so stop can report it.
func (e *antsExecutor) Release(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = 0
		}
		return e.pool.ReleaseTimeout(remaining)
	}
	return e.pool.ReleaseContext(ctx)
}

// antsLoggerAdapter adapts the plugin's log.Logger to ants.Logger's
// Printf(format string, args ...any) contract.
type antsLoggerAdapter struct {
	log log.Logger
}

func (a antsLoggerAdapter) Printf(format string, args ...any) {
	a.log.Info(fmt.Sprintf(format, args...))
}
