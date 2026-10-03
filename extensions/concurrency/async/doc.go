// Package async is a drained background task pool: XBC's analogue of
// Spring's applicationTaskExecutor plus @Async, together with its executor
// shutdown phase. Importing this package is side-effect free. Prefer explicit
// composition with async.Bundle(); executables that intentionally use
// process-wide autoload may blank-import the autoload subpackage.
//
// # Usage
//
// Selecting the Bundle both constructs one Pool as a Definition's primary
// value and, at Init, installs that Pool as the process-wide Spawner, so most
// callers need nothing beyond:
//
//	app, err := xbc.New(xbc.WithBundles(async.Bundle()))
//
//	func handleOrder(ctx context.Context, order Order) error {
//		if err := async.Spawn(ctx, "notify-order", func(taskCtx context.Context) {
//			notify(taskCtx, order)
//		}); err != nil {
//			// ErrSaturated or ErrShuttingDown: fall back to sending inline,
//			// or report the failure -- never spawn a bare goroutine instead.
//			notify(ctx, order)
//		}
//		return nil
//	}
//
// Spawn's ctx bounds only how long the call may wait for capacity, and
// supplies values (trace identifiers, etc.) to the task; it is not the
// task's own lifetime. A finished request's context does not cancel the task
// it spawned: the task receives context.WithoutCancel(ctx) combined with the
// Pool's own cancellation, so it keeps running after Spawn returns and still
// stops when the Pool itself stops.
//
// A plugin that would rather depend on the capability explicitly than reach
// for the global declares the same typed construction dependency every other
// consumer of a Definition's primary value does:
//
//	var spawnerInput = plugin.RefTo[async.Spawner](async.Key)
//
//	var definition = plugin.Define(
//		"order-service",
//		func(ctx plugin.BuildContext) (*OrderService, error) {
//			return &OrderService{spawner: spawnerInput.Get(ctx).Value}, nil
//		},
//		plugin.Options[*OrderService]{
//			Inputs: plugin.Inputs(spawnerInput),
//		},
//	)
//
// Exactly one Pool may be bound globally per process. A second Pool's Init
// (a second async.Definition instance, or a second App in the same process)
// leaves the first binding untouched and logs a warning instead of
// overwriting it; global async.Spawn always reaches the first.
//
// # Configuration
//
// Config is bound from plugins.async:
//
//	plugins:
//	  async:
//	    executor: goroutine          # only accepted value in this step
//	    max_concurrency: 256         # tasks running at once; 0 = unlimited
//	    queue_capacity: 1024         # tasks waiting once max_concurrency is reached; 0 = no queue
//	    submit_timeout: 0s           # how long Spawn waits for capacity; 0s = reject immediately
//	    shutdown:
//	      await_termination: true          # Drain waits for running and queued tasks
//	      await_termination_period: 0s     # extra cap on that wait; 0s = bounded only by xbc.drain_timeout
//
// Selecting the Bundle enables the Pool with these safe defaults; a
// deployment that wants it absent sets plugins.async.enabled: false.
//
// # Shutdown
//
// Drain and Stop are lifecycle the framework runs on this Pool through the
// Definition, not methods a caller invokes directly. Shutdown runs in XBC's
// usual three phases: ingress (every TrafficOpener and its dependents) stops
// first, then the framework drains every remaining participant that
// implements plugin.Drainer or declares a Drain lifecycle adapter --
// including this Pool -- in reverse start order within xbc.drain_timeout,
// then it stops everything not yet stopped.
//
// Draining stops this Pool from admitting new work (Spawn then returns
// ErrShuttingDown) and, if shutdown.await_termination is true, waits for
// running and queued tasks to finish, bounded by
// min(shutdown.await_termination_period, the drain ctx). An expired wait
// means draining stops waiting, never that it cancels anything: every task
// still outstanding when the wait expires keeps running, and the Pool logs
// what it abandoned (names, running time, queue depth) rather than silently
// losing track of it. Draining is idempotent, including its result, and is
// safe before Init/Start, after a failed Start, and more than once.
//
// Stopping cancels whatever outlived the drain, discards any still-queued
// tasks (logging their names), waits for the running goroutines it cancelled
// to actually return within its own ctx, releases the executor, and unbinds
// the process-global Spawner if this Pool was the one bound. It is correct
// whether draining ran, timed out, failed, or never ran, and it never
// re-reports a failure draining already returned.
//
// The guarantee this Pool offers is best-effort, not durable: a process that
// receives SIGKILL, hits an OOM kill, or crashes loses every task still
// running or queued, drain or no drain. Work that must survive that -- an
// email that must eventually send, a billing event that must eventually post
// -- belongs in extensions/messaging/outbox or extensions/jobs/asynq, not
// here. This Pool is for work whose loss on a hard crash is an acceptable
// cost for not persisting it: a cache warm, a best-effort notification, a
// metrics flush.
//
// Each task runs with its own panic recovery -- a panicking task is logged
// with its name and stack and never crashes the process -- and under a
// runtime/pprof label "async_task=<name>", so a CPU or goroutine profile can
// be read per task name.
package async
