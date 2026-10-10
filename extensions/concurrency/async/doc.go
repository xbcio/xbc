// Package async is a drained background task pool: XBC's analogue of
// Spring's applicationTaskExecutor plus @Async, together with its executor
// shutdown phase. Importing this package is side-effect free. Prefer explicit
// composition with async.Bundle(); executables that intentionally use
// process-wide autoload may blank-import the autoload subpackage.
//
// # Tasks
//
// Application work is usually expressed through extensions/tasks, the
// protocol-neutral task facade: a task defined once with tasks.New or
// tasks.Method is submitted with tasks.Submit, run with tasks.Run, or
// dispatched fire-and-forget with tasks.Go. Selecting this Bundle makes the
// Pool the process-wide local executor for those calls, collecting the
// tasks.Provider export of every plugin and every tasks.New definition; when
// asynq is selected as well, submissions go to it instead and this Pool keeps
// serving Run lookups, Go, and any remotely consumed task whose handler runs
// locally. Spawn and Spawner, the entry points below, remain the lower-level
// exit for work with no task handle -- loose closures and framework internals
// -- and both paths obey the same admission and shutdown behavior.
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
//	    executor: goroutine          # or "ants"; see "# Executors" below
//	    max_concurrency: 256         # tasks running at once; 0 = unlimited (ants requires > 0)
//	    queue_capacity: 1024         # tasks waiting once max_concurrency is reached; 0 = no queue
//	    submit_timeout: 0s           # how long Spawn waits for capacity; 0s = reject immediately
//	    shutdown:
//	      await_termination: true          # Drain waits for running and queued tasks
//	      await_termination_period: 0s     # extra cap on that wait; 0s = bounded only by xbc.drain_timeout
//	    ants:                               # only meaningful when executor: ants
//	      expiry_duration: 1s                # ants' own default idle-worker scan interval
//	      pre_alloc: false                   # ants' own default
//	      disable_purge: false               # ants' own default
//
// Selecting the Bundle enables the Pool with these safe defaults; a
// deployment that wants it absent sets plugins.async.enabled: false.
//
// # Workload quota
//
// Every task the Pool executes charges one unit of the workload the Pool itself
// belongs to (workloads.<key>.max_goroutines) for as long as it runs, the same
// budget the workload's managed tasks charge. MaxConcurrency bounds the pool's
// own workers, which is a per-pool number; the workload quota is what stops a
// pool from spending a whole workload's capacity on its own, and it is the
// reason the two are not the same knob. A task waits for a unit instead of
// being refused, so the pool keeps its accepted work and runs it at the
// workload's pace; a task still waiting when the Pool stops is abandoned and
// logged rather than run. A Pool belonging to no workload, or one whose
// workload declares no budget, charges nothing.
//
// The ants section's three fields are read only when executor is "ants": see
// AntsConfig's doc comment for why setting any of them away from its default
// while executor is "goroutine" is rejected at startup rather than silently
// ignored, and why that check cannot distinguish "written on purpose" from
// "never written" the way xbc.drain_timeout's own env.Exists check can.
//
// # Executors
//
// Both executors behind Config.Executor honor identical Pool semantics:
// the same MaxConcurrency cap, the same QueueCapacity and SubmitTimeout
// behavior, the same panic recovery and pprof labeling per task, and the
// same Drain/Stop contract. Switching Executor changes only how an admitted
// task is actually run, never what Spawn, Drain, or Stop promise a caller.
//
// ExecutorGoroutine (the default) starts every task on its own goroutine.
// It needs no further configuration and is the right choice unless profiling
// shows goroutine creation/teardown itself is a bottleneck.
//
// ExecutorAnts runs tasks on a github.com/panjf2000/ants/v2 pool sized to
// MaxConcurrency, reusing a fixed set of goroutines across tasks instead of
// spawning a fresh one per task. Prefer it when Spawn is called at a very
// high rate and profiling shows goroutine creation and the GC pressure from
// its stack allocation are a measurable cost: ants' worker reuse amortizes
// that cost across many tasks instead of paying it on every Spawn. It is not
// a default-safe upgrade -- benchmark your own workload (BenchmarkSpawn and
// BenchmarkSpawnQueued in this package compare both executors for a tiny
// task) before switching, since worker-reuse overhead can offset or exceed
// the saved allocation for short, infrequent, or already-cheap tasks.
//
// Measured on this package's own BenchmarkSpawn/BenchmarkSpawnQueued (Apple
// M3, go test -bench Spawn -benchmem -count=5, averaged), ExecutorGoroutine
// was consistently faster than ExecutorAnts for a tiny task in both an
// unsaturated run (BenchmarkSpawn: ~1.2us/op goroutine vs. ~1.3us/op ants)
// and a saturated, queueing one (BenchmarkSpawnQueued, MaxConcurrency 8:
// ~1.08us/op for both, no measurable difference). ants' worker reuse did
// not amortize its own overhead away for a task this cheap on this
// workload; do not read ExecutorAnts as a speedup for small tasks from
// these numbers -- it exists for workloads where goroutine creation and
// stack-allocation GC pressure are the measured bottleneck, which a
// microbenchmark this small does not reproduce.
//
// ants is given no exported MaxBlockingTasks/Nonblocking knob: Pool's own
// semaphore already guarantees at most MaxConcurrency tasks ever reach the
// executor, and queued tasks are run by the worker loop itself (see
// Pool.runWorker) rather than resubmitted to the executor, so the ants pool
// is left in its default blocking mode and Submit is expected to block only
// as briefly as the in-progress hand-off of the one worker it is always
// waiting on, never to deadlock (see antsExecutor.Go's own doc comment for
// the argument) or report overload. Its ReleaseContext (or ReleaseTimeout,
// if the remaining Stop ctx carries a deadline) runs in Stop, after Pool's
// own wait for in-flight work -- never in Drain, which must leave every
// resource a still-running task depends on open.
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
// safe before Init/Start, after a failed Start, and more than once. A Pool
// drained or stopped before its Init stays closed: Init then returns
// ErrDrainedBeforeInit instead of opening admission for a Pool every Spawn
// would reject.
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
