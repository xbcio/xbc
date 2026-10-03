package async

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/panjf2000/ants/v2"

	"github.com/xbcio/xbc/log"
)

// antsExecutor runs tasks on a fixed-size, goroutine-reusing worker pool
// backed by github.com/panjf2000/ants/v2. It implements the same executor
// seam as goroutineExecutor; Pool's own semaphore (MaxConcurrency) is the only
// admission control, so this pool's size always equals MaxConcurrency and its
// Submit must never itself block or queue.
//
// Go wraps every task so panic recovery and the per-task pprof label -- both
// otherwise provided by Pool.runTask for a one-task-per-goroutine executor --
// are unaffected by ants reusing the calling goroutine across tasks: without
// a label scoped to the single pprof.Do call inside the wrapper, a label set
// by one task could otherwise outlive it and be attributed to whatever task
// the same reused worker goroutine runs next.
type antsExecutor struct {
	pool *ants.Pool
}

// newAntsExecutor creates an ants pool sized to maxConcurrency. maxConcurrency
// must be positive; prepareConfig enforces that before an antsExecutor is ever
// constructed. The pool is created non-blocking: Submit returns ErrPoolOverload
// instead of blocking the caller once every worker is busy, which Go treats as
// an internal invariant violation (see Go's doc comment) because Pool never
// calls Go without having already reserved a running slot.
func newAntsExecutor(maxConcurrency int, cfg AntsConfig, logger log.Logger) (*antsExecutor, error) {
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("async: ants executor requires a positive max_concurrency, got %d", maxConcurrency)
	}
	if logger == nil {
		logger = log.Nop()
	}
	options := []ants.Option{
		ants.WithNonblocking(true),
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

// antsOverloadRetryBudget bounds how long Go retries a Submit that failed
// with ants.ErrPoolOverload before treating it as a genuine invariant
// violation. Pool's semaphore (p.running) always reserves a slot before Go
// is ever called, but ants itself only considers a worker reclaimed after
// the task function it ran has fully returned to the worker's own loop (see
// worker.go's goWorker.run): a queued task promoted from Pool.finishRunning
// can therefore reach Go a few scheduler ticks before ants' own bookkeeping
// catches up with the slot Pool already freed. That window is a scheduling
// artifact, not a capacity disagreement, and closes on its own almost
// immediately, so a short bounded retry absorbs it without masking an
// actual overload: a real one (which cannot happen given Pool's semaphore,
// short of a bug) would still exhaust the budget and surface as an error.
const antsOverloadRetryBudget = 50 * time.Millisecond

// Go submits fn to the ants pool. Because the pool is non-blocking and sized
// to MaxConcurrency, and Pool never calls Go without already having reserved
// one of those MaxConcurrency slots, a Submit that still reports
// ants.ErrPoolOverload after antsOverloadRetryBudget means the two trackers
// disagreed about available capacity for longer than the expected worker
// hand-off window: that is an internal invariant violation, not a
// saturation signal, so it is reported as an error (and logged by the
// caller) rather than translated into ErrSaturated.
func (e *antsExecutor) Go(fn func()) error {
	deadline := time.Now().Add(antsOverloadRetryBudget)
	for {
		err := e.pool.Submit(fn)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ants.ErrPoolOverload) || time.Now().After(deadline) {
			if errors.Is(err, ants.ErrPoolOverload) {
				return fmt.Errorf("async: ants pool rejected a task its own size should have admitted: %w", err)
			}
			return err
		}
		runtime.Gosched()
	}
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
