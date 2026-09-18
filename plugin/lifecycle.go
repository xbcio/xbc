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
