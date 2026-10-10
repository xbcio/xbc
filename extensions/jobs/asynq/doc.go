// Package asynq provides an XBC-managed Redis-backed task runtime. Importing
// this package is side-effect free; applications explicitly compose Bundle or
// opt in through the autoload subpackage.
//
// # Usage
//
// Define handlers as a canonical Plugin that explicitly exports
// HandlerContributor, then compose its Bundle with asynq.Bundle:
//
//	type mailHandlers struct{}
//
//	func (*mailHandlers) TaskHandlers() []asynq.HandlerRegistration {
//		return []asynq.HandlerRegistration{{
//			Type: "mail.send",
//			Handler: asynq.HandlerFunc(func(context.Context, asynq.Task) error {
//				return nil
//			}),
//		}}
//	}
//
//	var definition = plugin.Define(
//		"mail-handlers",
//		func(plugin.BuildContext) (*mailHandlers, error) { return &mailHandlers{}, nil },
//		plugin.Options[*mailHandlers]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[asynq.HandlerContributor](
//					func(handlers *mailHandlers) asynq.HandlerContributor { return handlers },
//				),
//			),
//		},
//	)
//
//	func Definition() plugin.Definition { return definition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(Definition()) }
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			asynq.Bundle(),
//			Bundle(),
//		))
//	}
//
// Asynq collects contributors as a typed input and freezes their handler
// registrations during construction. A separate producer Plugin can consume
// Enqueuer through a declared plugin.RefTo[asynq.Enqueuer](asynq.Key) input.
// Keeping enqueueing and handler contribution in separate Definitions avoids a
// dependency cycle.
//
// # Tasks
//
// Application work is usually expressed through extensions/tasks, the
// protocol-neutral task facade: a task defined once with tasks.New or
// tasks.Method is submitted with tasks.Submit, run with tasks.Run, or
// dispatched fire-and-forget with tasks.Go. Selecting this Bundle installs the
// Plugin as the process-wide remote executor at Init, so a submission is
// enqueued to Redis -- carrying the queue, retries, and timeout its name
// resolves to under plugins.asynq.tasks -- until Stop uninstalls it again.
// Enqueuer and HandlerContributor remain the lower-level exits for submissions
// and handlers that work with the vendor types directly.
//
// A tasks.Provider export is consumed exactly like a HandlerContributor: the
// providing Plugin's workload decides which worker runs its tasks, under that
// worker's queues, concurrency, and admission quota. A tasks.New task is
// consumed only where plugins.asynq.tasks.<name>.consume: true says so, on the
// unowned worker: defining a task in a package a process imports must not make
// that process start consuming it, or every enqueue-only process would quietly
// join the queue. A consumed task's queue must be one its consuming worker
// fetches; pointing one at a queue the worker does not consume is refused at
// construction rather than left to sit in Redis. Queue, retry count, and
// timeout come from deployment configuration:
//
//	plugins:
//	  asynq:
//	    tasks:
//	      mail.send_confirmation:
//	        queue: mail          # defaults to default_queue
//	        max_retries: 3       # defaults to default_max_retries
//	        timeout: 2m          # defaults to default_timeout
//	      stats.recount:
//	        consume: true        # run this tasks.New task on the unowned worker
//
// Entries under plugins.asynq.tasks are keyed by task name, which has no
// environment-variable spelling, so they are configured in a file: a variable
// such as XBC_PLUGINS_ASYNQ_TASKS_MAIL_TIMEOUT is refused at startup as naming
// no configuration field rather than applied. The section-wide defaults
// (XBC_PLUGINS_ASYNQ_DEFAULT_TIMEOUT and its siblings) remain overridable.
//
// A failure the handler marks with tasks.Permanent, and a payload the binding
// cannot decode, are not retried: both are returned to the queue as SkipRetry
// and the task is archived after its first attempt. The match is by
// errors.Is, so a handler that returns a wrapped tasks.ErrPayload from a
// chained Submit whose argument would not encode is archived the same way:
// an encoding failure does not heal on redelivery. Every other handler
// failure follows the task's configured retry policy.
//
// # Workloads
//
// A worker serves the handlers of the Plugins that share a workload. A
// contributor whose Plugin belongs to a workload is consumed by that workload's
// worker, with the queues and concurrency declared under
// plugins.asynq.workloads.<key>; contributors that belong to none share the
// top-level worker and its top-level queues. Which process consumes a queue is
// therefore a matter of which workloads it hosts, and a workload this process
// does not host gets no worker here even though its queues stay valid for
// enqueuing. The queue sets of one process must be pairwise disjoint, because a
// task in a queue two workers both poll is delivered to whichever fetches it
// first; that is refused at construction.
//
// Every delivery charges one unit of its workload's admission quota, the same
// quota the workload's managed tasks charge, so what a workload's
// max_goroutines bounds is the work it runs rather than the goroutines XBC
// started for it. A delivery that cannot take a unit waits for one within the
// task's own context rather than failing the task; contributors that belong to
// no workload, and workloads that declare no quota, charge nothing.
//
// A handler runs under the same profiler label the runtime files its managed
// tasks under -- workload=<key> -- so a CPU profile taken in a process serving
// several workloads can be read per workload rather than only per task type.
// The label is set for the duration of the handler and restored afterwards,
// since the queue library reuses its worker goroutines. Contributors that
// belong to no workload stay unlabelled, exactly as the runtime leaves unowned
// plugins unlabelled: a label naming no workload is the truth about the shared
// work, and a misattributed sample is worse than an unattributed one.
//
// A process whose Plugins contribute no handlers is not an error: it starts,
// consumes nothing, and provides the long-lived task the runtime needs, which
// is what a standby role needs to stay alive until it is restarted with work.
//
// # Queues and shutdown
//
// Queue names passed to Queue must exist in plugins.asynq, either in the
// top-level queues or in any workload's. The runtime owns its Redis connection,
// enqueue client, and worker servers. Start submits one critical managed task
// per worker, each waiting for XBC's global traffic gate before polling Redis.
// Shutdown splits in two. Drain stops every worker from fetching new tasks and
// waits, within the drain budget, for the handlers already running to return,
// while Enqueue and the owned Redis connection stay usable. That wait is
// best-effort at the fetch boundary: asynq does not wait for a worker that
// already dequeued a task, so a task dequeued just before the worker stopped may
// begin after the wait and is left to Stop's library Shutdown, which finishes or
// requeues it instead of losing it. An expired drain means stop waiting, never
// abort, so handlers it leaves behind keep running with their contexts
// untouched. Stop then rejects new enqueue calls and shuts the workers down
// through the asynq library, which lets whatever outlived the drain finish
// within plugins.asynq.shutdown_timeout before requeueing the rest, and closes
// Redis only once the workers stopped. Tasks may be redelivered, so handlers
// should be idempotent and payloads should not contain unprotected secrets.
//
// # Readiness
//
// The primary Plugin exports health.Contributor and contributes the reachability
// of the Redis instance it owns as a readiness check named "asynq". Init pings
// that connection once; without the check, losing it afterwards stays invisible
// until an Enqueue call fails or the worker quietly stops consuming. A plugin
// that has not initialized, or that has stopped, reports down rather than a probe
// defect. The contribution is inert unless the application also selects the
// health capability Bundle.
package asynq
