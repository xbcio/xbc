package runtime

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// taskRuntime is the managed-goroutine runtime behind Context.Go and
// Context.GoCritical. It owns three things a plain sync.WaitGroup cannot
// express: a global admission gate, one cancellable task context per plugin
// instance, and the critical-failure escalation path.
//
// # Why admission is a state, not a convention
//
// The classic WaitGroup shutdown bug is Add racing Wait: one goroutine calls
// wg.Add while another is already inside wg.Wait, and Wait either returns
// before the new task has run or the runtime panics outright ("WaitGroup
// misuse: Add called concurrently with Wait"). Documentation-level discipline
// ("just don't submit tasks during shutdown") cannot fix it, because the
// submitting goroutine is a plugin's own code -- often the tail of its own
// Stop -- and has no way to observe when shutdown started.
//
// So admission is a flag guarded by the same mutex as the Add, and closing it
// is the first thing unwind does, before any Stop is called. Once
// closeAdmission returns, no further Add is reachable: every submit takes mu,
// sees accepting == false, and refuses without touching any WaitGroup. Every
// later Wait is therefore guaranteed to run after the final Add. The race is
// eliminated structurally rather than avoided by timing.
//
// A refused submission is reported, never silently dropped -- a plugin that
// spawns a "report final state" goroutine on the way out would otherwise
// believe it reported something.
//
// # Why one task context per plugin instance
//
// Shutdown walks plugins in reverse dependency order and, for each one, calls
// Stop *while that plugin's own tasks are still running*, then cancels that
// plugin's context and waits for them. A single process-wide task context
// could not express that: cancelling everything up front would pull the rug
// out from under exactly the goroutines a graceful Stop depends on. An HTTP
// server drains in-flight requests inside Stop, and those requests are being
// served by its own managed serving goroutine -- kill it first and there is
// nothing left to drain.
type taskRuntime struct {
	mu        sync.Mutex
	accepting bool
	spawned   int
	groups    map[plugin.Identity]*pluginTasks

	// shuttingDown tells a returning critical task whether its return was
	// requested. It is deliberately application-level, not per-plugin.
	//
	// Deriving it from the plugin's own task context -- as the older
	// implementation did, testing whether the run context was cancelled --
	// contradicts the shutdown order above: a consumer that stops pulling
	// inside its Stop makes its critical goroutine return at a moment when
	// its context has deliberately *not* been cancelled yet. The per-plugin
	// test would read that as "nobody asked it to stop, yet it stopped",
	// raise a spurious critical failure, and turn a textbook-clean shutdown
	// into exit code 1. This flag is set once, at the very start of unwind,
	// before any Stop runs, so "requested" means requested.
	shuttingDown atomic.Bool

	logger     log.Logger
	onCritical func(reason string)
}

// pluginTasks is one plugin instance's managed tasks: a context that cancels
// only its own goroutines, and the WaitGroup that tracks them.
type pluginTasks struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newTaskRuntime builds a task runtime whose critical failures escalate
// through onCritical.
func newTaskRuntime(logger log.Logger, onCritical func(reason string)) *taskRuntime {
	return &taskRuntime{
		accepting:  true,
		groups:     make(map[plugin.Identity]*pluginTasks),
		logger:     logger,
		onCritical: onCritical,
	}
}

// submit runs fn as a managed goroutine owned by id, returning false when the
// runtime has already stopped admitting work.
//
// critical selects between the two failure policies. For a non-critical task a
// panic is recovered and logged and the goroutine simply ends -- the right
// default for one-shot work like a cache refresh or a metrics push, where "it
// stopped" is not itself bad news. For a critical task both a panic and an
// unprompted normal return escalate through onCritical: a consumer loop that
// quietly returns has stopped consuming, and the process still being
// technically alive is precisely what makes that failure mode dangerous.
//
// A task's context is derived from context.Background rather than from
// whatever stage happened to spawn it: managed tasks routinely outlive Init,
// and must stop for exactly one reason -- their own plugin being shut down.
func (r *taskRuntime) submit(id plugin.Identity, fn func(context.Context), critical bool) bool {
	r.mu.Lock()
	if !r.accepting {
		r.mu.Unlock()
		r.log().Warn("xbc: 任务组已关闭，该任务未启动",
			"plugin", id.String(), "critical", critical)
		return false
	}

	g, ok := r.groups[id]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		g = &pluginTasks{ctx: ctx, cancel: cancel}
		r.groups[id] = g
	}
	g.wg.Add(1)
	r.spawned++
	r.mu.Unlock()

	go func() {
		defer g.wg.Done()

		// requested records whether the eventual return (if fn does not
		// panic) happened because shutdown was already underway. Written
		// after fn returns, read only by the deferred func below in this same
		// goroutine -- no concurrent access.
		requested := false

		defer func() {
			if rec := recover(); rec != nil {
				r.log().Error("xbc: 托管 goroutine panic，已恢复",
					"plugin", id.String(),
					"panic", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()))
				if critical {
					r.onCritical(fmt.Sprintf("插件 %s 的托管 goroutine panic：%v", id, rec))
				}
				return
			}
			if critical && !requested {
				r.onCritical(fmt.Sprintf("插件 %s 的托管 goroutine 意外提前返回", id))
			}
		}()

		fn(g.ctx)
		requested = r.shuttingDown.Load()
	}()
	return true
}

// accepts reports whether the runtime still admits new tasks. It backs
// Context.TasksAccepted through the host adapter.
func (r *taskRuntime) accepts() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepting
}

// spawnedCount returns how many tasks have ever been admitted. The liveness
// check reads it -- see App.assertLiveness for why "ever admitted" is the
// answerable question and "running right now" is not.
func (r *taskRuntime) spawnedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawned
}

// closeAdmission stops the runtime admitting new tasks and raises the
// shutdown flag. Idempotent, and the mandatory first step of unwind: it must
// happen before any Closer.Stop runs, both so that no Add can still race a
// later Wait and so that a task returning because its plugin's Stop asked it
// to is recognised as having been asked.
func (r *taskRuntime) closeAdmission() {
	r.shuttingDown.Store(true)
	r.mu.Lock()
	r.accepting = false
	r.mu.Unlock()
}

// stopPlugin cancels one plugin instance's tasks and waits for them, bounded
// by the shared shutdown deadline. It is called immediately after that
// plugin's Stop returned, so the tasks were still alive for the whole of
// Stop -- which is what lets Stop drain through them.
//
// The group is removed from the map as it is handled, so cancelAll and
// drainRemaining below only ever deal with what is genuinely left.
func (r *taskRuntime) stopPlugin(id plugin.Identity, deadline context.Context) error {
	r.mu.Lock()
	g, ok := r.groups[id]
	delete(r.groups, id)
	r.mu.Unlock()
	if !ok {
		return nil
	}

	g.cancel()
	return waitTasks(g, id.String(), deadline)
}

// cancelAll cancels every task context not yet handled, without waiting.
//
// This is the budget-exhaustion escape hatch: once the shared deadline is
// spent, every remaining plugin is going to be reported as timed out anyway,
// so telling all of their tasks to stop right now at least gives them the
// duration of the remaining Stop calls to wind down, instead of being
// cancelled one by one behind Stops that no longer wait for anything.
func (r *taskRuntime) cancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.groups {
		g.cancel()
	}
}

// drainRemaining cancels and waits for any task group that the per-instance
// walk did not reach.
//
// It exists because ownership of a task and membership of the initialized
// set are not the same thing: a plugin whose Init failed part-way may already
// have submitted a task before failing. Leaving such a group unwaited would
// let a goroutine outlive the unwind that was supposed to account for it.
func (r *taskRuntime) drainRemaining(deadline context.Context) []error {
	r.mu.Lock()
	leftovers := make(map[plugin.Identity]*pluginTasks, len(r.groups))
	for id, g := range r.groups {
		leftovers[id] = g
	}
	r.groups = make(map[plugin.Identity]*pluginTasks)
	r.mu.Unlock()

	var errs []error
	for id, g := range leftovers {
		g.cancel()
		if err := waitTasks(g, id.String(), deadline); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// waitTasks waits for one group's tasks, giving up when the shared deadline
// is spent.
//
// The bound is not optional. A task that ignores its context is a plugin bug,
// but an unbounded Wait would turn that bug into a process that never exits
// and has to be SIGKILLed, losing every remaining plugin's Stop with it. The
// abandoned goroutines are deliberately leaked -- the same trade stopBounded
// makes for a Stop that never returns, and for the same reason: the process
// is about to exit, so the leak costs nothing and the alternative costs the
// operational promise that this process stops within its budget.
//
// Waiting in a goroutine is safe here without any Add/Wait race precisely
// because admission was closed before unwind called any of this: no further
// Add is reachable.
func waitTasks(g *pluginTasks, owner string, deadline context.Context) error {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-deadline.Done():
	}

	// Look once more before declaring a timeout: the tasks may have finished
	// in the same instant the budget expired, and reporting a plugin that did
	// stop cleanly as one that hung would send the operator chasing a bug
	// that is not there.
	select {
	case <-done:
		return nil
	default:
		return fmt.Errorf(
			"xbc: 插件 %s 的托管任务未在关闭预算内退出，已放弃等待\n"+
				"  → 检查传给 ctx.Go/ctx.GoCritical 的函数是否真的监听了 ctx.Done()", owner)
	}
}

// log returns the runtime's logger, falling back to the global one so a
// runtime built before logging was configured never panics on its first
// message.
func (r *taskRuntime) log() log.Logger {
	if r.logger == nil {
		return log.L()
	}
	return r.logger
}
