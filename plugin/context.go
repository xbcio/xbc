package plugin

import (
	"context"
	"time"

	"github.com/xbcio/xbc/log"
)

// RuntimeHost is the narrow runtime port exposed through Context. It carries
// no value publication, lookup, or initialized-plugin enumeration.
type RuntimeHost interface {
	ExecutionContext() context.Context
	Logger() log.Logger
	TrafficGate() <-chan struct{}
	SubmitTask(id Identity, fn func(context.Context), critical bool) bool
	RequestShutdown(id Identity, reason string) bool
}

// Context is the lifecycle-operation parameter for one Plugin instance. It
// exposes only cancellation, identity, logging, Start-scoped task admission,
// and shutdown request; configuration and dependency wiring belong elsewhere.
type Context struct {
	host RuntimeHost
	id   Identity
}

var _ context.Context = (*Context)(nil)

// NewRuntimeContext is framework assembly API. Ordinary Plugins receive a
// Context from lifecycle methods and never construct one: the host passed here
// owns the execution context the Context reports, so a plugin building its own
// would run lifecycle work against a host the runtime does not own. An
// architecture guard keeps the runtime the only production caller.
func NewRuntimeContext(host RuntimeHost, id Identity) *Context {
	return &Context{host: host, id: id.Normalized()}
}

func (c *Context) executionContext() context.Context {
	if c == nil || c.host == nil {
		return context.Background()
	}
	ctx := c.host.ExecutionContext()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (c *Context) Deadline() (time.Time, bool) { return c.executionContext().Deadline() }
func (c *Context) Done() <-chan struct{}       { return c.executionContext().Done() }
func (c *Context) Err() error                  { return c.executionContext().Err() }
func (c *Context) Value(key any) any           { return c.executionContext().Value(key) }

func (c *Context) Name() string       { return c.id.Plugin.String() }
func (c *Context) Key() Key           { return c.id.Plugin }
func (c *Context) Instance() string   { return c.id.Instance }
func (c *Context) Identity() Identity { return c.id }

func (c *Context) Log() log.Logger {
	if c == nil || c.host == nil || c.host.Logger() == nil {
		return log.L()
	}
	return c.host.Logger()
}

// TrafficGate remains closed throughout fallible traffic preparation and is
// closed exactly once by the runtime after every participant succeeds. A
// Start-owned serving task waits on this channel before exposing ingress.
func (c *Context) TrafficGate() <-chan struct{} {
	if c == nil || c.host == nil {
		return nil
	}
	return c.host.TrafficGate()
}

// Go submits a non-critical managed task. Submission is accepted only while
// this Plugin's Start hook is executing; false means the admission window is
// closed.
func (c *Context) Go(fn func(context.Context)) bool {
	return c != nil && c.host != nil && c.host.SubmitTask(c.id, fn, false)
}

// GoCritical submits a task whose panic or unprompted return requests
// application shutdown.
func (c *Context) GoCritical(fn func(context.Context)) bool {
	return c != nil && c.host != nil && c.host.SubmitTask(c.id, fn, true)
}

func (c *Context) RequestShutdown(reason string) bool {
	return c != nil && c.host != nil && c.host.RequestShutdown(c.id, reason)
}
