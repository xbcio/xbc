package xbc

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
)

// Context is the per-instance handle a plugin receives from Init onward. It
// carries just enough identity (name/instance) to pre-bind the logger; the
// registry accessor, config accessor and managed-goroutine methods land in
// later tasks (Task 4, Task 6, Task 12 respectively).
type Context struct {
	app      *App
	name     string // plugin name
	instance string // instance name, always non-empty ("default" by default)
	logger   log.Logger
}

// Log returns a Logger pre-bound with this instance's identity. Task 4 wires
// up the actual field-binding (plugin=/instance=); for now it just hands back
// whatever logger the instance carries, falling back to the global logger so
// a Context built without one (e.g. in a unit test) never panics.
func (c *Context) Log() log.Logger {
	if c.logger == nil {
		return log.L()
	}
	return c.logger
}

// Name returns the plugin name this Context belongs to.
func (c *Context) Name() string { return c.name }

// Instance returns the instance name ("default" unless the plugin is
// multi-instance and configured otherwise).
func (c *Context) Instance() string { return c.instance }

// registry returns the App-wide (type, instance) store this Context's App owns.
// Unexported: Provide/Get/GetNamed/MustGet/MustGetNamed are the public seam.
func (c *Context) registry() *registry {
	return c.app.registry
}

// Go hands fn to the framework as a managed background goroutine.
// See goroutine.go for the full semantics of Go vs GoCritical.
func (c *Context) Go(fn func(context.Context)) {
	c.app.goManaged(c, fn, false)
}

// GoCritical is Go's stricter twin: a panic or an unprompted return
// triggers a full application shutdown. See goroutine.go for the full
// semantics of Go vs GoCritical.
func (c *Context) GoCritical(fn func(context.Context)) {
	c.app.goManaged(c, fn, true)
}

// Route reports which frozen route table entry gc is currently handling, by
// matching its method and gin-assigned FullPath. It returns nil before
// stage 7 has run (no router yet) and nil for a request that somehow
// matches nothing in the table -- middleware loaded in stage 7 runs before
// any route exists (spec §4.1 constraint g), so this request-time lookup is
// the only way a middleware can ever learn which route it is wrapping.
func (c *Context) Route(gc *gin.Context) *RouteInfo {
	if c.app.router == nil {
		return nil
	}
	full := gc.FullPath()
	for _, r := range *c.app.router.routes {
		if r.Method == gc.Request.Method && r.Path == full {
			info := r
			return &info
		}
	}
	return nil
}

// Routes returns the full frozen route table. It is only meaningful after
// stage 7's freeze -- PostRoutes (which runs right after freeze) is the
// intended caller, for plugins like swagger or a casbin policy sync that
// need every route at once rather than one at a time.
func (c *Context) Routes() []RouteInfo {
	if c.app.router == nil {
		return nil
	}
	return *c.app.router.routes
}
