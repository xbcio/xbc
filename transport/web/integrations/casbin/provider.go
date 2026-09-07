package casbin

import (
	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/persist"
)

// EnforcerProvider exposes the live synchronized enforcer without making a
// concrete pointer type an XBC contract. Callers retain ordinary Casbin APIs
// (including GetAdapter) while the boolean prevents use after lifecycle stop.
type EnforcerProvider interface {
	Enforcer() (*casbinlib.SyncedEnforcer, bool)
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
