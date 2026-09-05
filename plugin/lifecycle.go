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
