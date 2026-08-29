package plugin

import (
	"context"
	"sync"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
)

// Context is the per-instance handle a plugin receives from Init onward. It
// stays deliberately neutral (see the package's protocol-agnostic design):
// Definition key/instance identity, execution-scoped context cancellation, a
// read-only config.View, a pre-bound log.Logger, managed-goroutine submission,
// and the registry/extension seams -- nothing that only a Gin- or gRPC-shaped
// application could use. Route()/Routes() are intentionally NOT here:
// request-time route lookup lives in web.CurrentRoute and the full route table
// in web.RouteCatalog, so a plugin that only ever talks gRPC is never forced
// to depend on Gin types through this Context.
type Context struct {
	host   RuntimeHost
	id     Identity
	env    config.View
	logger log.Logger

	// lifecycle is bound by the framework after assembly and before the first
	// startup hook. Keeping it behind a lock makes the zero value useful in
	// plugin unit tests while also making cancellation reads race-free.
	lifecycleMu sync.RWMutex
	lifecycle   context.Context
}

var _ context.Context = (*Context)(nil)

// BindLifecycleContext attaches the execution-scoped context observed by
// Init, Migrate, Start, and OpenTraffic. This is framework assembly API, not a
// plugin extension point: xbc binds it once before invoking any startup hook.
// A nil parent is normalized to context.Background so Context's zero-value
// cancellation behavior remains safe and predictable.
func BindLifecycleContext(ctx *Context, parent context.Context) {
	if ctx == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx.lifecycleMu.Lock()
	ctx.lifecycle = parent
	ctx.lifecycleMu.Unlock()
}

func (c *Context) executionContext() context.Context {
	c.lifecycleMu.RLock()
	ctx := c.lifecycle
	c.lifecycleMu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Deadline, Done, Err, and Value make *Context a standard context.Context.
// Startup hooks that may block must cooperate with cancellation by selecting
// on Done (or passing this Context to context-aware dependencies). The
// framework never runs a hook in a detached goroutine merely to interrupt it.
func (c *Context) Deadline() (time.Time, bool) { return c.executionContext().Deadline() }
func (c *Context) Done() <-chan struct{}       { return c.executionContext().Done() }
func (c *Context) Err() error                  { return c.executionContext().Err() }
func (c *Context) Value(key any) any           { return c.executionContext().Value(key) }

// Name returns Key as text for logging and display convenience.
func (c *Context) Name() string { return c.id.Plugin.String() }

// Key returns the stable key of the owning Definition.
func (c *Context) Key() Key { return c.id.Plugin }

// Instance returns the instance name ("default" unless the owning Definition
// has MultipleInstances and is configured otherwise). Always non-empty --
// NewRuntimeContext
// normalizes it once, at construction, so every subsequent read here is
// guaranteed non-empty without re-checking.
func (c *Context) Instance() string { return c.id.Instance }

// Identity returns the (Definition key, instance) pair this Context was built with.
func (c *Context) Identity() Identity { return c.id }

// Log returns a Logger pre-bound with this instance's identity, falling
// back to the global logger so a Context built without one (e.g. in a unit
// test) never panics.
func (c *Context) Log() log.Logger {
	if c.logger == nil {
		return log.L()
	}
	return c.logger
}

// Config returns a read-only view onto the merged configuration
// environment. Plugins read their own section through this; they do not get
// a mutable handle onto the framework's configuration tree.
func (c *Context) Config() config.View { return c.env }

// Go hands fn to the host's managed task group as a non-critical background
// task: a panic is recovered and logged, and an unprompted return is not
// itself treated as a failure.
func (c *Context) Go(fn func(context.Context)) {
	c.host.GoManaged(c.id, fn, false)
}

// GoCritical is Go's stricter twin: a panic or an unprompted return is
// treated as the application having failed and triggers a full shutdown.
// Use it for goroutines whose death means the application is no longer
// doing its job even though the process is still up -- a consumer loop, a
// long-lived gateway connection, a data sync loop.
func (c *Context) GoCritical(fn func(context.Context)) {
	c.host.GoManaged(c.id, fn, true)
}

// taskAdmissionReporter is an optional capability a RuntimeHost may implement to
// let Context.TasksAccepted report whether the managed task group is still
// admitting new work. It is deliberately NOT part of the RuntimeHost interface
// itself: RuntimeHost's method set is pinned at four methods (see host.go), and
// admission status can be obtained by probing for this narrower, optional
// interface instead of growing RuntimeHost to a fifth method that most RuntimeHost
// implementations (and every test double) would otherwise be forced to
// carry even when they have no admission gate to report on.
type taskAdmissionReporter interface{ TasksAccepted() bool }

// TasksAccepted reports whether the host's managed task group still accepts
// new Go/GoCritical submissions. A host that has not begun shutting down
// (or that simply does not implement the optional admission-reporting
// capability at all) is assumed to accept tasks, so plugins that never call
// this during a normal run see no behavioral difference.
func (c *Context) TasksAccepted() bool {
	if r, ok := c.host.(taskAdmissionReporter); ok {
		return r.TasksAccepted()
	}
	return true
}
