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
// # Retries
//
// A failed handler is redelivered after retry_backoff_base, doubling on every
// later attempt and capped by retry_backoff_cap (both durations; defaults 1s
// and 60s). A handler that knows when its work should be rechecked returns
// asynq.RetryAfter(err, delay): the delay wins over the backoff, is recognized
// through errors.As, and therefore survives further wrapping. A reported delay
// is still a failed attempt and does not exempt the task from its max_retries
// budget, so the queue library archives a task that keeps deferring itself
// once that budget is exhausted. When a task type's cluster-wide limit is full,
// the delivery is deferred with asynq.ErrClusterConcurrencyLimit and
// cluster_concurrency_retry_delay; that deferral is logged at WARN because it
// is capacity behavior rather than a fault.
//
// # Cluster-wide concurrency
//
// A task entry may bound how many handlers of its type run at once across the
// whole cluster, not just within one worker:
//
//	plugins:
//	  asynq:
//	    tasks:
//	      report.render:
//	        cluster_concurrency: 4        # 0 (default) leaves the type unbounded
//	        cluster_concurrency_lease: 10m # defaults to the task's timeout
//
// The cap covers both sources of a handler -- a tasks.New binding this process
// consumes and a HandlerContributor registration of the same type -- because it
// is applied where the dispatcher routes a delivery, not where the handler was
// registered. A delivery that cannot take a slot returns
// asynq.ErrClusterConcurrencyLimit wrapped in asynq.RetryAfter with
// cluster_concurrency_retry_delay (default 5s), so the task returns to the
// queue without running; it consumes a retry attempt like any other failure.
//
// The implementation lives in the framework-free kit/distributed package: the
// slots of one task type live in the Redis sorted set distributed:sem:<task
// type>. Its members are per-execution tokens scored by their lease expiry in
// Unix milliseconds. Acquisition discards expired tokens before counting, a
// running handler renews its token every lease/3, and the key is set to expire
// after twice the lease, so a replica that dies frees its slots when the leases
// lapse instead of blocking the type forever. When renewal finds the token gone,
// the handler's context is cancelled: the slot no longer protects the work that
// was admitted under it. If the key holds a Redis type other than a sorted set,
// acquisition and reset delete and recreate it, recovering a key claimed by an
// unrelated writer. Controller's ResetSemaphore(taskType) removes expired tokens
// and leaves live ones alone, so it is safe to call during a rolling restart.
//
// The key prefix belongs to the library rather than to plugins.asynq.
// Versions before this migration wrote a different layout
// (xbc:asynq:sem:<task type>); while old and new processes run together, the
// two semaphore families are mutually invisible, so one task type is limited
// once per layout rather than once cluster-wide until every process runs this
// version.
//
// # Retention
//
// default_retention keeps a successful task's result metadata for the
// configured duration when its submission passes no Retention option; zero,
// the default, leaves the queue library's behavior unchanged and removes a
// successful task. An explicit Retention option, including Retention(0),
// overrides the configured default for that submission.
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
// # Inspection and control
//
// Two further contracts expose queue management. They are separate on purpose,
// so a process that only reads queue state never receives the ability to mutate
// it: Inspector is read-only (Queues, QueueInfo, ListTasks, GetTaskInfo) and
// Controller is destructive (DeleteTask, DeleteAllByState, RunTask,
// ArchiveTask, CancelProcessing, PauseQueue, UnpauseQueue, PurgeArchived). A
// consumer declares the one it needs with plugin.RefTo:
//
//	inspector := plugin.RefTo[asynq.Inspector](asynq.Key)
//	controller := plugin.RefTo[asynq.Controller](asynq.Key)
//
// Both contracts expose only this package's own value types -- State,
// QueueInfo, Page, and the extended TaskInfo -- and never a queue-library type,
// so upgrading the library does not change the consumer API. State has one
// value per queue-library task state (pending, active, scheduled, retry,
// archived, completed); StateUnknown reports anything outside that vocabulary.
//
// Both contracts are scoped to the queues this plugin declares under
// plugins.asynq: the top-level queues set plus every workload's queues. An
// operation on any other queue fails with ErrQueueNotOwned before Redis is
// touched, so a management endpoint cannot reach another process's queues by
// mistake. This is scope, not authorization.
//
// WARNING: authorization is the consuming application's responsibility. This
// contract performs no authentication and no access control beyond the
// declared-queue check; exposing Controller through an operator endpoint
// without an authorization policy lets any caller pause, purge, or delete the
// declared queues. Enforce the application's own permission model on that
// endpoint.
//
// Errors are this package's sentinels and are meant to be tested with
// errors.Is: ErrQueueNotFound, ErrTaskNotFound, ErrQueueNotEmpty,
// ErrQueueNotOwned, ErrInvalidState, and ErrUnsupportedState. After Stop every
// method reports ErrClosed. Controller's individual calls are bound only by the
// Redis client's configured dial/read/write timeouts; the library's Inspector
// is not context-aware, so these methods take no context and a cancelled
// caller context cannot interrupt an operation already handed to Redis.
//
// PurgeArchived(queue, olderThan, taskTypes...) deletes archived tasks whose
// archive time is at or before olderThan and reports how many it deleted; the
// zero time purges the whole archive. With no task types it considers every
// task; with one or more it deletes only archived tasks of those exact types,
// reading each candidate's metadata because the archive itself records no
// type. The archive time is the archived sorted set's score, read
// from the queue library's Redis keys, because TaskInfo.LastFailedAt is the
// last handler failure and is zero or stale for tasks archived through the
// Inspector. A queue Redis has not registered reports ErrQueueNotFound.
// CancelProcessing takes no queue argument, so it first locates the task in the
// declared queues and publishes nothing when none contains it.
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
// # TLS
//
// The owned Redis connection can speak TLS:
//
//	plugins:
//	  asynq:
//	    redis:
//	      addr: "redis.internal:6379"
//	      tls:
//	        ca_file: /etc/xbc/redis/ca.pem
//	        cert_file: /etc/xbc/redis/client.pem   # optional, for mTLS
//	        key_file: /etc/xbc/redis/client-key.pem
//	        server_name: redis.internal
//	        min_version: "1.2"
//
// The section is off by default and the connection stays in cleartext until one
// of ca_file, cert_file, key_file, server_name, or insecure_skip_verify is set.
// ca_file replaces the system roots; cert_file and key_file must be configured
// together. min_version accepts "1.2" or "1.3" only. The material is read while
// the Plugin is constructed, so an unreadable file fails construction rather
// than the first enqueue or delivery. The enqueue client and every worker share
// one connection, so one section covers both directions.
// insecure_skip_verify disables certificate verification: it exists for
// development endpoints and must not be used in production.
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
