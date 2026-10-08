package casbin

import (
	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/persist"
)

// EnforcerProvider exposes the live synchronized enforcer together with the
// request convention that enforcer's model was built and validated for,
// without making a concrete pointer type an XBC contract. Callers retain
// ordinary Casbin APIs (including GetAdapter) while the boolean prevents use
// after lifecycle stop. The enforcer remains the exposing plugin's to own for
// the life of the process: callers must not retain it after lifecycle
// shutdown, publish it as process-global state, or close the resources its
// owner still manages.
type EnforcerProvider interface {
	// Enforcer returns the live synchronized enforcer while the plugin is
	// active, and (nil, false) once its Stop has begun.
	Enforcer() (*casbinlib.SyncedEnforcer, bool)
	// RequestConvention reports the convention the engine validated its model
	// against, so a consumer can build the request tuple Enforce expects
	// without reading the enforcer's model. An implementation must report the
	// shape its model, policies, and matcher were built for: the engine
	// refuses its own models that disagree with the configured convention, so
	// this value and the live enforcer cannot contradict each other. Reading
	// the model instead is not safe while the enforcer is live --
	// SyncedEnforcer.GetModel returns the field LoadPolicy replaces under its
	// own lock -- so this is the interface through which a consumer learns the
	// shape, and it stays valid for the plugin's lifetime, Stop included.
	RequestConvention() RequestConvention
}

// AdapterProvider supplies one externally owned Casbin policy adapter. The
// provider remains responsible for adapter-specific construction and schema
// migration; Plugin owns neither the provider nor its backing connection.
type AdapterProvider interface {
	Adapter() persist.Adapter
}

// WatcherFactory opens a watcher for one live enforcer. Plugin owns every
// successfully returned watcher and closes it exactly once during shutdown or
// failed startup. Implementations must clean up resources acquired before an
// error is returned.
type WatcherFactory interface {
	Open(*casbinlib.SyncedEnforcer) (persist.Watcher, error)
}

// ProviderRef selects one exact plugin capability exporter. An empty Plugin
// disables the capability; an empty Instance selects plugin.DefaultInstance.
type ProviderRef struct {
	Plugin   string `yaml:"plugin"`
	Instance string `yaml:"instance" default:"default"`
}
