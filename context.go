package xbc

import "github.com/xbcio/xbc/log"

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
