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

	// execCtx is cancelled by stop and is the cancellation every running
	// task's context is derived from in addition to its own detached parent.
	execCtx    context.Context
	execCancel context.CancelFunc

	// inFlight tracks every task handed to the executor, running or queued,
	// so drain and stop can wait for it without separate bookkeeping.
	inFlight sync.WaitGroup

	// shutdownOnce guards the stop sequence so it never runs twice and
	// nothing is released twice, matching AGENTS.md's shared once-guard
	// requirement for the Drain/Stop split.
	shutdownOnce sync.Once
	stopErr      error

	// drainClaimed/drainErr mirror elasticsearch's cached-error ownership:
	// the first drain call to observe the wait's outcome owns reporting it,
	// and every later drain call -- concurrent or sequential -- replays that
	// same value instead of re-waiting, keeping drain idempotent including
	// its result.
	drainClaimed bool
	drainErr     error
}

var _ Spawner = (*Pool)(nil)

// newPreparedPool constructs a directly usable Pool with the given
// configuration and opens admission immediately, matching the Definition's
// Init-time admission opening. It does not bind the process-global Spawner;
// the Definition does that through initPool. A Pool built this way is not
// started, drained, or stopped by anyone unless its caller wires it into a
// Lifecycle itself (as the Definition does): it exists for tests that need a
// ready-to-use Pool without going through full plugin construction, not as a
// supported way to run a Pool outside the framework.
func newPreparedPool(cfg Config, logger log.Logger) (*Pool, error) {
	prepared, err := prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := newPool(prepared, logger)
	if err != nil {
		return nil, err
	}
	pool.open()
	return pool, nil
}

// newPool constructs a Pool with its configured executor. cfg is expected to
// have already passed prepareConfig/validate; newPool itself only fails if
// building the configured executor fails (currently only possible for
// ExecutorAnts, whose pool construction can reject an invalid ants.* value
// prepareConfig did not already catch).
func newPool(cfg Config, logger log.Logger) (*Pool, error) {
	if logger == nil {
		logger = log.Nop()
	}
	exec, err := newExecutor(cfg, logger)
	if err != nil {
		return nil, err
	}
	execCtx, execCancel := context.WithCancel(context.Background())
	return &Pool{
		cfg:        cfg,
		log:        logger,
		now:        time.Now,
		exec:       exec,
		execCtx:    execCtx,
		execCancel: execCancel,
		changed:    make(chan struct{}),
	}, nil
}

// newExecutor builds the executor named by cfg.Executor.
func newExecutor(cfg Config, logger log.Logger) (executor, error) {
	switch cfg.Executor {
	case ExecutorAnts:
		return newAntsExecutor(cfg.MaxConcurrency, cfg.Ants, logger)
	default:
		return newGoroutineExecutor(), nil
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

	taskCtx := p.taskContext(ctx)
	outcome, err := p.admit(ctx, queuedTask{name: name, taskCtx: taskCtx, task: task, queuedAt: p.now()})
	if err != nil {
		return err
	}
	// admit already called p.inFlight.Add(1) for this task inside the same
	// locked section that granted it a running or queued slot, so Add is
	// guaranteed to happen-before any inFlight.Wait call drain or stop can
	// start afterward: a Wait that starts when the counter is (transiently)
	// zero can never race a positive Add from a Spawn that admit already
	// let through. The same locked section also already appended the task
	// to p.queue itself when it decided to queue rather than run it (see
	// admit's own doc comment for why that decision and the append cannot
	// be split across two lock acquisitions), so admitQueued needs no
	// further action here.
	if outcome == admitRunning {
		if err := p.exec.Go(func() { p.runWorker(name, taskCtx, task) }); err != nil {
			p.inFlight.Done()
			p.releaseOrHandoff(name)
			return fmt.Errorf("async: executor rejected task %q: %w", name, err)
		}
	}
	return nil
}

type admitOutcome int

const (
	admitRunning admitOutcome = iota
	admitQueued
)

// admit decides whether this Spawn call may proceed, reserving a running slot
// or a queue slot before returning. It also calls p.inFlight.Add(1) inside the
// same locked section that grants the slot, so the Add is guaranteed to
// happen-before any inFlight.Wait call a concurrent drain or stop starts: a
// Spawn that admit lets through can never be invisible to a Wait that begins
// immediately afterward, which is the hazard sync.WaitGroup's own contract
// warns against ("calls with a positive delta that start when the counter is
// zero must happen before a Wait").
//
// When it decides to queue rather than run entry, admit appends entry to
// p.queue itself, inside the very same locked section as that decision,
// rather than returning admitQueued and letting Spawn append separately
// under a second lock acquisition. Splitting it across two critical
// sections would open a gap between "admit decided to queue" and "the task
// actually landed in p.queue" during which a worker's own nextOrRelease
// (also taken under p.mu) could observe an empty queue, release its running
// slot, and return -- abandoning this task forever, since nothing else is
// looking at the queue once every worker has exited. Appending inside the
// same critical section that made the decision closes that gap: by the
// time admit's lock is released, the task is already wherever a concurrent
// nextOrRelease will look for it.
//
// SubmitTimeout 0 rejects immediately with ErrSaturated the moment neither a
// running nor a queue slot is available, regardless of ctx's own deadline:
// the configured wait is the effective ceiling, never widened by a caller's
// longer-lived ctx. A positive SubmitTimeout instead waits up to
// min(SubmitTimeout, ctx's own deadline); if ctx itself is the earlier
// deadline and it fires first, admit returns ctx.Err() rather than
// ErrSaturated, so "the spawn ctx deadline shorter than submit_timeout wins".
func (p *Pool) admit(ctx context.Context, entry queuedTask) (admitOutcome, error) {
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
			p.inFlight.Add(1)
			p.signalChangedLocked()
			p.mu.Unlock()
			return admitRunning, nil
		}
		if len(p.queue) < p.cfg.QueueCapacity {
			p.inFlight.Add(1)
			p.queue = append(p.queue, entry)
			p.signalChangedLocked()
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

// nextOrRelease is the single atomic decision point between a worker
// picking up another queued task and releasing its running slot: it must
// run as one locked step, not two, because admit's own "is a slot free"
// check (also taken under p.mu) would otherwise be able to interleave
// between "the queue looked empty" and "the slot was released" and queue a
// task no worker is left looking for. Under one lock acquisition: if the
// pool has stopped, the worker must stop popping (any remaining queued
// tasks are stop's to discard, not this worker's to run) and release its
// slot; otherwise, if the queue is non-empty, the next task is popped and
// the slot stays reserved for the caller to keep running with; otherwise
// the slot is released in the same critical section that observed the
// empty queue, so no concurrent admit can ever queue a task behind a
// worker that has already decided to exit.
func (p *Pool) nextOrRelease() (queuedTask, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped && len(p.queue) > 0 {
		next := p.queue[0]
		p.queue = p.queue[1:]
		p.signalChangedLocked()
		return next, true
	}
	if p.running > 0 {
		p.running--
	}
	p.signalChangedLocked()
	return queuedTask{}, false
}

// releaseOrHandoff frees the running slot a task never actually started in
// (exec.Go itself failed, which Pool treats as an invariant violation; see
// antsExecutor.Go) and, in the same spirit as nextOrRelease, hands that slot
// to a queued task instead of silently abandoning it if one is waiting.
// Unlike nextOrRelease, there is no live worker goroutine to keep looping on
// here -- the task that would have started this goroutine's runWorker never
// got to -- so a queued task picked up this way is submitted to the
// executor as a fresh worker rather than run inline. A second exec.Go
// failure recurses rather than looping, which is safe: Pool treats every
// such failure as a rare invariant violation, and nextOrRelease's own
// stopped/empty-queue checks still bound how many times this can happen.
func (p *Pool) releaseOrHandoff(failedName string) {
	next, ok := p.nextOrRelease()
	if !ok {
		return
	}
	name, taskCtx, task := next.name, next.taskCtx, next.task
	if err := p.exec.Go(func() { p.runWorker(name, taskCtx, task) }); err != nil {
		p.log.Error("async: executor rejected queued task while recovering a failed submission",
			"name", name, "failedTask", failedName, "error", err.Error())
		p.inFlight.Done()
		p.releaseOrHandoff(name)
	}
}

// taskContext builds the context a running task receives: it carries ctx's
// values (so trace identifiers survive) but neither ctx's cancellation (a
// finished request must not cancel work it merely scheduled) nor an already
// expired deadline, combined with the Pool's own cancellation so stop can
// still terminate the task.
func (p *Pool) taskContext(ctx context.Context) context.Context {
	detached := context.WithoutCancel(ctx)
	combined, cancel := context.WithCancel(detached)
	context.AfterFunc(p.execCtx, cancel)
	return combined
}

// runWorker is the body an executor goroutine runs for one admitted task and
// then, in a loop, for every queued task it picks up itself afterward. It is
// the sole place a task actually executes.
//
// After the first task returns, the same goroutine -- still holding its
// running slot -- checks the queue itself (nextOrRelease) instead of
// releasing the slot and relying on a new Submit/Spawn to pick the next task
// up: a queued task is guaranteed a worker without ever asking the executor
// for one, so ants (or any future executor) never needs to admit more
// workers than max_concurrency to drain its own queue, and no extra
// goroutine is created per task. Only once the queue is empty, or the pool
// has stopped, does the loop release its slot and let the goroutine return
// to its executor (idle for goroutineExecutor, reclaimed by ants for
// antsExecutor).
//
// Each iteration gets its own panic recovery, pprof label, inFlight.Done,
// and Stats accounting, exactly as a one-task-per-goroutine executor would
// provide for each task individually.
func (p *Pool) runWorker(name string, ctx context.Context, task func(context.Context)) {
	for {
		p.runOne(name, ctx, task)
		next, ok := p.nextOrRelease()
		if !ok {
			return
		}
		name, ctx, task = next.name, next.taskCtx, next.task
	}
}

// runOne executes a single task with panic recovery and a pprof label, then
// marks it done in inFlight. It never touches p.running: the caller
// (runWorker) owns the running slot for as long as it keeps picking up
// queued work.
func (p *Pool) runOne(name string, ctx context.Context, task func(context.Context)) {
	defer p.inFlight.Done()
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

// drain stops admitting new work and, if configured, waits for running and
// queued tasks to finish. It never cancels running work or discards the
// queue; that is stop's job. See doc.go for the full shutdown sequence.
//
// drain is idempotent, including its result: a drain call that observes the
// wait finish (or that it never needed to wait) owns the result, and every
// later drain call -- concurrent or sequential -- replays that same result
// rather than re-waiting. drain is also safe before Init/Start and after stop
// has already run.
func (p *Pool) drain(ctx context.Context) error {
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
// every later call, so drain's result is idempotent and stop never has to
// recompute or repeat it.
func (p *Pool) claimDrainResult(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped && !p.drainClaimed {
		// stop ran first, or raced this call to completion: drain
		// contributes nothing of its own to report, and stop's own result
		// (if any) is reported through stop, not replayed here.
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

// stop cancels whatever outlived the drain, discards any still-queued tasks,
// waits for running goroutines within ctx, releases the executor, and unbinds
// the process-global Spawner if this Pool is bound. stop is correct whether
// drain ran, timed out, failed, or never ran at all, and never repeats a
// failure drain already returned.
func (p *Pool) stop(ctx context.Context) error {
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
				p.inFlight.Done() // discarded, never ran: it will not reach runOne's Done.
			}
			p.log.Warn("async: stop discarded queued tasks", "count", len(discardedQueue), "tasks", names)
		}

		// Cancel whatever outlived the drain (or everything, if drain never
		// ran) before waiting: stop must not wait forever for work drain
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
