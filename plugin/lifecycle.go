package plugin

import "context"

// Initializer performs synchronous initialization after the factory's
// successful return has transferred ownership to XBC. Stop remains eligible
// even when Init fails or panics.
type Initializer interface{ Init(ctx *Context) error }

// Migrator performs optional migration work after all enabled instances have
// initialized.
type Migrator interface{ Migrate(ctx *Context) error }

// Runner performs synchronous startup. Managed task submission is accepted
// only while this method is executing for the owning Plugin.
type Runner interface{ Start(ctx *Context) error }

// TrafficOpener performs fallible traffic preparation behind the global gate.
// It must not expose ingress; the runtime releases one gate after every
// participant succeeds.
type TrafficOpener interface{ OpenTraffic(ctx *Context) error }

// Closer stops an owned primary value. It must be safe after any partially
// completed lifecycle state and is called at most once by XBC.
type Closer interface {
	Stop(ctx context.Context) error
}

// PreStopper retracts an owned primary value's externally visible
// participation before any Stop begins, and is called at most once by XBC. It
// is XBC's counterpart of a Kubernetes container's preStop hook, and it exists
// for the one thing Stop cannot express: Stop is the point of no return, while
// a value that has leased a role, published an address, or registered itself
// for work usually has to become invisible to its peers while it is still
// fully alive. Every Stop in the application starts only after every PreStop
// has returned, been abandoned, or been skipped.
//
// It is called only for an instance that completed the start phase, so a
// startup that failed before or during Start runs no PreStop at all: there was
// nothing to retract yet.
//
// The context carries only the pre-stop budget (xbc.pre_stop_timeout). It is
// deliberately neither the application's execution context, which the stop
// request has already cancelled by the time this runs, nor a *Context, whose
// Done and Err delegate to that same cancelled context. A hook that reached
// for either would fail on its first remote call with context.Canceled, and
// because a failed PreStop is only logged, nothing downstream would say so:
// the silent release failure would read exactly like a successful one.
//
// A returned error is logged and changes nothing else. Stop still runs and the
// process still exits cleanly, because a shutdown already under way has no
// better outcome to offer.
type PreStopper interface {
	PreStop(ctx context.Context) error
}

// Drainer finishes accepted in-flight work while every dependency it may call
// into is still running, and is called at most once by XBC. It runs after the
// process has stopped accepting ingress and before any non-ingress Stop: the
// ingress closure (every TrafficOpener and everything that transitively
// depends on one) has already been stopped, so no new external request can
// reach a Drainer, and everything it depends on is still running. Drainers run
// one at a time in reverse start order, so a Drainer's dependents that are
// Drainers themselves have already drained -- and may have handed it
// follow-up work -- before it is drained, while its non-Drainer dependents are
// still running and may still call it. Its managed tasks are also still
// running: a task scope is cancelled only after its owner's Stop. This is
// XBC's counterpart of Spring's executor SmartLifecycle phase, which stops
// after the web container's graceful shutdown and before bean destruction.
//
// Drain must stop admitting new work, wait for work it already accepted, and
// return either when that work has finished or when ctx expires, whichever
// comes first. An expired ctx means "stop waiting", not "abort": cancelling
// work that is still running, and releasing what that work uses, is Stop's
// job. Drain must not close a resource that its own in-flight work, or a
// dependent's, may still use -- a producer, a connection pool, a transport --
// and must be safe to call before Start, after a failed Start, and more than
// once. ctx carries only the drain deadline (xbc.drain_timeout): it is
// derived from Background, exactly as PreStop's context is, and for the same
// reason -- the execution context is already cancelled by the time this runs,
// and a hook wired to it would fail its first check with context.Canceled.
//
// A returned error, a panic, or a Drain that ignores its deadline is recorded
// and reported, and never stops the unwind: Drain hands work over voluntarily,
// and the Stop phase that follows must be correct whether or not Drain ran or
// succeeded -- it still has to finish, or abandon, whatever Drain left behind.
// A failure Drain already returned should not be returned by Stop again.
type Drainer interface {
	Drain(ctx context.Context) error
}
