// Package tasks defines a task once and runs it from anywhere in the process:
// synchronously in the caller's goroutine, or asynchronously through whichever
// executor plugin the application selected. The code that defines, submits,
// and consumes a task never names the plugin underneath -- async is the local
// executor, asynq the remote one -- so switching from an in-process pool to a
// Redis-backed queue is a change to the application's Bundle list, not to the
// task code.
//
// This is a contract module: it depends only on the standard library and owns
// no Definition, Config, or Bundle.
//
// # Usage
//
// A function task is defined once, as a package-level handle:
//
//	var Recount = tasks.New("stats.recount", func(ctx context.Context, shard int) error {
//		return stats.Recount(ctx, shard)
//	})
//
// A task that needs plugin dependencies takes them from a method on the
// plugin, which declares the task by binding it:
//
//	var SendConfirmation = tasks.Method("mail.send_confirmation", (*Service).sendConfirmation)
//
//	func (s *Service) Tasks() []tasks.Binding {
//		return tasks.Bind(s, SendConfirmation)
//	}
//
// The plugin exports tasks.Provider in its Definition
// (plugin.ExportAs[tasks.Provider]), which makes every executor in the
// application collect the binding. From any request:
//
//	mail.SendConfirmation.Submit(ctx, mail.Confirmation{OrderID: id}) // asynchronous
//	mail.SendConfirmation.Run(ctx, mail.Confirmation{OrderID: id})    // synchronous
//	Recount.Submit(ctx, 3)
//
// Loose background work needs no definition at all:
//
//	tasks.Go(ctx, func(ctx context.Context) { cache.Refresh(ctx, uid) })
//
// Argument types are derived from the handler signature and may be any
// JSON-encodable type. Run and Submit check them at compile time.
//
// # Task classes
//
//	New(name, fn)          Method(name, (*S).m)
//	dependencies           none                 a plugin value, injected by the framework
//	runnable locally       as soon as defined   once the providing plugin is selected
//	remote consumption     opt-in by name       follows the providing plugin's selection
//	shutdown ordering      unconstrained        executor stops after the providing plugin
//
// A function task is consumed remotely only when the application opts it in by
// name in the remote executor's configuration; a method task is consumed
// wherever the plugin that provides it is selected. This asymmetry is
// deliberate: a process that merely imports a definition package -- a Web tier
// that only submits -- must not silently start competing for queued work.
//
// # Semantics of Run, Submit, and Go
//
// Run executes in the caller's goroutine and returns the handler's error; it
// never encodes the argument, never occupies executor capacity, and never
// waits on a remote executor. A panic in the handler is recovered and
// returned as an error. A method task whose plugin is not part of the process
// reports ErrNoHandler.
//
// Submit encodes the argument and hands it to the installed executor: the
// remote one when present, otherwise the local one, otherwise ErrNotInstalled.
// It reports acceptance, not completion. Errors from the executor are
// preserved for errors.Is.
//
// Go always goes to the local executor and takes its name from the function
// symbol unless Named overrides it. It reports ErrNotInstalled in an
// application with only a remote executor: a function cannot be delivered
// over a queue.
//
// The submission context bounds only how long the call waits to be accepted.
// A locally accepted task keeps the submission context's values but not its
// cancellation, so a finished request does not cancel the work it submitted;
// a remotely accepted task carries no context values at all.
//
// # Local and remote differences
//
// The two executors do not promise identical semantics; design for the one the
// deployment selects:
//
//	                      local (async)              remote (asynq)
//	error / panic         logged, no retry            retried per task configuration; Permanent is not
//	delivery              at most once                at least once -- handlers must be idempotent
//	context values        preserved                   not carried across the queue
//	no handler present    Submit reports ErrNoHandler consumer logs the miss and retries or archives
//	payload               used in-process and gone   stored in Redis across versions: evolve compatibly
//
// # Definition rules
//
// New and Method claim their name for the process and panic on an empty name,
// surrounding whitespace, or a name another definition owns -- the same
// fail-at-startup discipline as http.HandleFunc. Definitions belong at package
// level: a handle defined per call would panic on the second call.
//
// # Errors
//
// ErrNotInstalled   no executor is installed (neither async nor asynq selected)
// ErrClosed         the executor a call needed is shutting down or uninstalled
// ErrSaturated      the local pool had no capacity within its submit timeout
// ErrNoHandler      this process holds no handler for the task
// ErrPayload        an argument could not be encoded or decoded
// ErrPermanent      marks (via Permanent) a failure a remote executor must not retry
//
// # Executors
//
// Executor plugins install themselves at Init through InstallLocal and
// InstallRemote and collect the task bindings they dispatch: Providers from
// the composition graph, and Funcs for function tasks. The two slots are
// independent and first come first served; an executor that loses the race
// leaves the installed one untouched. See the async and asynq package
// documentation for their configuration and shutdown behavior.
package tasks
