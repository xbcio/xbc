package async

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/xbcio/xbc/log"
)

// Errors returned by Spawn and Pool.Spawn.
var (
	// ErrNotInstalled is returned by the package-level Spawn when no Pool is
	// bound. It never falls back to a bare goroutine.
	ErrNotInstalled = errors.New("async: no pool is installed")
	// ErrShuttingDown is returned once a Pool has started draining or has
	// stopped.
	ErrShuttingDown = errors.New("async: pool is shutting down")
	// ErrSaturated is returned when neither a running slot nor queue space
	// became available within the effective wait.
	ErrSaturated = errors.New("async: pool is saturated")
	// ErrInvalidTask is returned for a missing name or a nil task.
	ErrInvalidTask = errors.New("async: task name is required and task must be non-nil")
)

// Spawner runs a named task in the background. ctx bounds only how long Spawn
// may wait for capacity and supplies values (trace identifiers, etc.) to the
// task; it does not bound the task's own lifetime. The task receives
// context.WithoutCancel(ctx) combined with the Pool's own cancellation, so a
// finished request does not cancel the task it spawned, while the task still
// stops when the Pool itself stops.
type Spawner interface {
	Spawn(ctx context.Context, name string, task func(context.Context)) error
}

// Stats is a snapshot of a Pool's live accounting.
type Stats struct {
	Running uint64
	Queued  uint64
}

// queuedTask is one task accepted for execution but waiting for a running
// slot to free.
type queuedTask struct {
	name     string
	taskCtx  context.Context
	task     func(context.Context)
	queuedAt time.Time
}

// Pool is a drained background task pool: the Spring applicationTaskExecutor
// analogue described in doc.go. The zero value is not ready for use; obtain
// one from Definition-based construction or New.
type Pool struct {
	cfg Config
	log log.Logger
	now func() time.Time

	exec executor

	mu sync.Mutex
	// changed is closed and replaced under mu every time running or queue
	// occupancy changes (or shutdown begins), so a Spawn call parked in admit
	// can select on it alongside its own wait deadline. A plain channel swap
	// avoids a condition variable, which cannot be waited on alongside a
	// context without a helper goroutine per call.
	changed chan struct{}
	running uint64
	queue   []queuedTask

	admitting bool // true once admission has opened (Init, or New)
	draining  bool
	stopped   bool

	// execCtx is cancelled by Stop and is the cancellation every running
	// task's context is derived from in addition to its own detached parent.
	execCtx    context.Context
	execCancel context.CancelFunc

	// inFlight tracks every task handed to the executor, running or queued,
	// so Drain and Stop can wait for it without separate bookkeeping.
	inFlight sync.WaitGroup

	// shutdownOnce guards the Stop sequence so it never runs twice and
	// nothing is released twice, matching AGENTS.md's shared once-guard
	// requirement for the Drain/Stop split.
	shutdownOnce sync.Once
	stopErr      error

	// drainClaimed/drainErr mirror elasticsearch's cached-error ownership:
	// the first Drain call to observe the wait's outcome owns reporting it,
	// and every later Drain call -- concurrent or sequential -- replays that
	// same value instead of re-waiting, keeping Drain idempotent including
	// its result.
	drainClaimed bool
	drainErr     error
}

var _ Spawner = (*Pool)(nil)

// New constructs a directly usable Pool with the given configuration and
// opens admission immediately, matching the Definition's Init-time admission
// opening. New does not bind the process-global Spawner; use the Definition
// for that.
func New(cfg Config) (*Pool, error) {
	prepared, err := prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool := newPool(prepared, log.L())
	pool.open()
	return pool, nil
}

func newPool(cfg Config, logger log.Logger) *Pool {
	if logger == nil {
		logger = log.Nop()
	}
	execCtx, execCancel := context.WithCancel(context.Background())
	return &Pool{
		cfg:        cfg,
		log:        logger,
		now:        time.Now,
		exec:       newGoroutineExecutor(),
		execCtx:    execCtx,
		execCancel: execCancel,
		changed:    make(chan struct{}),
	}
}

// open allows Spawn to admit work. It is idempotent. It does nothing once
// draining or stopped has already been set, so a Pool drained before Start
// refuses to (re)open admission.
func (p *Pool) open() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draining || p.stopped {
		return
	}
	p.admitting = true
}

// Stats returns a snapshot of the Pool's current running and queued task
// counts.
func (p *Pool) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Running: p.running, Queued: uint64(len(p.queue))}
}

// signalChangedLocked wakes every goroutine parked on the previous changed
// channel. p.mu must be held.
func (p *Pool) signalChangedLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Spawn implements Spawner. See the Spawner doc comment for the task context
// contract.
func (p *Pool) Spawn(ctx context.Context, name string, task func(context.Context)) error {
	if p == nil {
		return ErrNotInstalled
	}
	if ctx == nil {
		return fmt.Errorf("async: Spawn requires a non-nil context")
	}
	if name == "" || task == nil {
		return ErrInvalidTask
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	outcome, err := p.admit(ctx)
	if err != nil {
		return err
	}

	taskCtx := p.taskContext(ctx)
	p.inFlight.Add(1)
	switch outcome {
	case admitRunning:
		if err := p.exec.Go(func() { p.runTask(name, taskCtx, task) }); err != nil {
			p.finishRunning()
			p.inFlight.Done()
			return fmt.Errorf("async: executor rejected task %q: %w", name, err)
		}
	case admitQueued:
		p.mu.Lock()
		p.queue = append(p.queue, queuedTask{name: name, taskCtx: taskCtx, task: task, queuedAt: p.now()})
		p.signalChangedLocked()
		p.mu.Unlock()
		go p.dispatchQueue()
	}
	return nil
}

type admitOutcome int

const (
	admitRunning admitOutcome = iota
	admitQueued
)

// admit decides whether this Spawn call may proceed, reserving a running slot
// or a queue slot before returning. SubmitTimeout 0 rejects immediately with
// ErrSaturated the moment neither is available, regardless of ctx's own
// deadline: the configured wait is the effective ceiling, never widened by a
// caller's longer-lived ctx. A positive SubmitTimeout instead waits up to
// min(SubmitTimeout, ctx's own deadline); if ctx itself is the earlier
// deadline and it fires first, admit returns ctx.Err() rather than
// ErrSaturated, so "the spawn ctx deadline shorter than submit_timeout wins".
func (p *Pool) admit(ctx context.Context) (admitOutcome, error) {
	waitCtx := ctx
	hasWait := p.cfg.SubmitTimeout > 0
	if hasWait {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, p.cfg.SubmitTimeout)
		defer cancel()
	}

	for {
		p.mu.Lock()
		if !p.admitting || p.draining || p.stopped {
			p.mu.Unlock()
			return 0, ErrShuttingDown
		}
		if p.cfg.MaxConcurrency == 0 || p.running < uint64(p.cfg.MaxConcurrency) {
			p.running++
			p.signalChangedLocked()
			p.mu.Unlock()
			return admitRunning, nil
		}
		if len(p.queue) < p.cfg.QueueCapacity {
			p.mu.Unlock()
			return admitQueued, nil
		}
		changed := p.changed
		p.mu.Unlock()

		if !hasWait {
			return 0, ErrSaturated
		}

		select {
		case <-changed:
			continue
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, ErrSaturated
		}
	}
}

// finishRunning frees one running slot and dispatches the next queued task
// into it, if any; otherwise it wakes any Spawn call parked in admit waiting
// for capacity.
func (p *Pool) finishRunning() {
	p.mu.Lock()
	if p.running > 0 {
		p.running--
	}
	p.signalChangedLocked()
	p.mu.Unlock()
	p.dispatchQueue()
}

// dispatchQueue starts queued tasks while a running slot is free. It is safe
// to call concurrently and when the queue is empty.
func (p *Pool) dispatchQueue() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 || !(p.cfg.MaxConcurrency == 0 || p.running < uint64(p.cfg.MaxConcurrency)) {
			p.mu.Unlock()
			return
		}
		next := p.queue[0]
		p.queue = p.queue[1:]
		p.running++
		p.signalChangedLocked()
		p.mu.Unlock()

		name, taskCtx, task := next.name, next.taskCtx, next.task
		if err := p.exec.Go(func() { p.runTask(name, taskCtx, task) }); err != nil {
			p.log.Error("async: executor rejected queued task", "name", name, "error", err.Error())
			p.inFlight.Done()
			p.finishRunning()
			continue
		}
	}
}

// taskContext builds the context a running task receives: it carries ctx's
// values (so trace identifiers survive) but neither ctx's cancellation (a
// finished request must not cancel work it merely scheduled) nor an already
// expired deadline, combined with the Pool's own cancellation so Stop can
// still terminate the task.
func (p *Pool) taskContext(ctx context.Context) context.Context {
	detached := context.WithoutCancel(ctx)
	combined, cancel := context.WithCancel(detached)
	context.AfterFunc(p.execCtx, cancel)
	return combined
}

// runTask executes one task with panic recovery and a pprof label, then frees
// its running slot.
func (p *Pool) runTask(name string, ctx context.Context, task func(context.Context)) {
	defer p.inFlight.Done()
	defer p.finishRunning()
	defer func() {
		if recovered := recover(); recovered != nil {
			p.log.Error("async: task panicked",
				"name", name,
				"panic", fmt.Sprintf("%v", recovered),
				"stack", string(debug.Stack()),
			)
		}
	}()
	pprof.Do(ctx, pprof.Labels("async_task", name), func(taskCtx context.Context) {
		task(taskCtx)
	})
}

// Drain stops admitting new work and, if configured, waits for running and
// queued tasks to finish. It never cancels running work or discards the
// queue; that is Stop's job. See doc.go for the full shutdown sequence.
//
// Drain is idempotent, including its result: a Drain call that observes the
// wait finish (or that it never needed to wait) owns the result, and every
// later Drain call -- concurrent or sequential -- replays that same result
// rather than re-waiting. Drain is also safe before Init/Start and after Stop
// has already run.
func (p *Pool) Drain(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.drainClaimed {
		err := p.drainErr
		p.mu.Unlock()
		return err
	}
	alreadyStopped := p.stopped
	p.draining = true
	awaitTermination := p.cfg.Shutdown.AwaitTermination
	period := p.cfg.Shutdown.AwaitTerminationPeriod
	p.signalChangedLocked()
	p.mu.Unlock()

	if alreadyStopped || !awaitTermination {
		return p.claimDrainResult(nil)
	}

	waitCtx := ctx
	if period > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, period)
		defer cancel()
	}

	done := make(chan struct{})
	go func() {
		p.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return p.claimDrainResult(nil)
	case <-waitCtx.Done():
		p.logAbandonedTasks()
		if ctx.Err() != nil {
			return p.claimDrainResult(ctx.Err())
		}
		return p.claimDrainResult(waitCtx.Err())
	}
}

// claimDrainResult records the first observed drain outcome and replays it on
// every later call, so Drain's result is idempotent and Stop never has to
// recompute or repeat it.
func (p *Pool) claimDrainResult(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped && !p.drainClaimed {
		// Stop ran first, or raced this call to completion: Drain
		// contributes nothing of its own to report, and Stop's own result
		// (if any) is reported through Stop, not replayed here.
		return nil
	}
	if !p.drainClaimed {
		p.drainClaimed = true
		p.drainErr = err
	}
	return p.drainErr
}

func (p *Pool) logAbandonedTasks() {
	p.mu.Lock()
	running := p.running
	queued := len(p.queue)
	names := make([]string, 0, queued)
	for _, entry := range p.queue {
		names = append(names, entry.name)
	}
	p.mu.Unlock()
	if running == 0 && queued == 0 {
		return
	}
	p.log.Warn("async: drain deadline expired with work still outstanding",
		"running", running,
		"queued", queued,
		"queuedTasks", names,
	)
}

// Stop cancels whatever outlived the drain, discards any still-queued tasks,
// waits for running goroutines within ctx, releases the executor, and unbinds
// the process-global Spawner if this Pool is bound. Stop is correct whether
// Drain ran, timed out, failed, or never ran at all, and never repeats a
// failure Drain already returned.
func (p *Pool) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.draining = true
		p.stopped = true
		discardedQueue := p.queue
		p.queue = nil
		p.signalChangedLocked()
		p.mu.Unlock()

		if len(discardedQueue) > 0 {
			names := make([]string, 0, len(discardedQueue))
			for _, entry := range discardedQueue {
				names = append(names, entry.name)
				p.inFlight.Done() // discarded, never ran: it will not reach runTask's Done.
			}
			p.log.Warn("async: stop discarded queued tasks", "count", len(discardedQueue), "tasks", names)
		}

		// Cancel whatever outlived the drain (or everything, if Drain never
		// ran) before waiting: Stop must not wait forever for work Drain
		// already gave up on.
		p.execCancel()

		done := make(chan struct{})
		go func() {
			p.inFlight.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			p.stopErr = ctx.Err()
		}

		if releaseErr := p.exec.Release(ctx); releaseErr != nil && p.stopErr == nil {
			p.stopErr = releaseErr
		}

		unbindGlobal(p)
	})
	return p.stopErr
}
