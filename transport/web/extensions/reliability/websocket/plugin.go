package websocket

import (
	"sync"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable plugin identity. The plugin has no configuration section:
// Options describe one endpoint and are supplied to Handler by the application
// rather than bound from the environment.
const Key plugin.Key = "websocket"

// problemCodeServiceUnavailable is the machine-readable code on the 503 an
// upgrade receives while the plugin is draining. It is the framework's
// service-unavailable vocabulary for a process that is shutting down, not a
// code shared with the health or readiness surfaces: those report a status
// document rather than an RFC 9457 problem.
const problemCodeServiceUnavailable = "service_unavailable"

// Plugin accepts WebSocket connections for the routes an application
// registers. It owns the registry of live connections so Drain and Stop can
// end them during shutdown, which http.Server.Shutdown cannot do once a
// connection has been hijacked.
type Plugin struct {
	// mu guards the draining flag and the registry together. One lock is what
	// makes "check the flag, then register" a single decision: a handler that
	// passes the pre-upgrade check but finishes its handshake after Drain took
	// the snapshot still observes the flag under the same lock and is closed
	// without being served.
	mu       sync.Mutex
	draining bool
	conns    map[*Conn]*connection
	logger   log.Logger
}

var (
	_ Upgrader       = (*Plugin)(nil)
	_ plugin.Drainer = (*Plugin)(nil)
	_ plugin.Closer  = (*Plugin)(nil)
)

var definition = plugin.Define(
	Key,
	func(context plugin.BuildContext) (*Plugin, error) {
		return newPlugin(context.Log()), nil
	},
	plugin.Options[*Plugin]{
		// The plugin is always active when its Bundle is composed. It has no
		// configuration section to activate it from, and a composition that
		// selected the Bundle deliberately asked for the capability.
		Exports: plugin.Contracts(
			plugin.ExportAs[Upgrader](func(value *Plugin) Upgrader { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the plugin's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the plugin's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// New constructs a plugin directly, outside the Definition-driven composition,
// with a no-op logger. Application composition should resolve the Upgrader
// exported by Bundle instead: the lifecycle drains the instance it
// constructed, and a directly constructed plugin is nobody's to drain.
func New() *Plugin { return newPlugin(log.Nop()) }

func newPlugin(logger log.Logger) *Plugin {
	if logger == nil {
		logger = log.Nop()
	}
	return &Plugin{conns: make(map[*Conn]*connection), logger: logger}
}

func (p *Plugin) log() log.Logger {
	if p == nil || p.logger == nil {
		return log.Nop()
	}
	return p.logger
}
