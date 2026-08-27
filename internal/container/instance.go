// Package container is the assembly pipeline: it turns a frozen
// plugin/catalog.Snapshot into live plugin instances, wired together by
// dependency order, config binding, and a shared (type, instance) value
// registry.
package container

import (
	"github.com/xbcio/xbc/internal/container/inject"
	"github.com/xbcio/xbc/plugin"
)

const defaultInstance = plugin.DefaultInstance

// Instance is one expanded plugin instance. Its identity and cardinality are
// copied from the immutable Definition and never inferred from the live Go
// value, so implementation types can be reused by unrelated definitions.
type Instance struct {
	plugin   plugin.Plugin
	key      plugin.Key
	multiple bool
	instance string
	ctx      *plugin.Context

	// fields is the xbc-tag scan of this instance's plugin, filled by resolve.
	fields []inject.FieldSpec

	deps     plugin.Deps
	provides []plugin.Dep
}

// Identity returns the stable definition key and normalized instance name.
func (i *Instance) Identity() plugin.Identity {
	return plugin.Identity{Plugin: i.key, Instance: i.instance}
}

// Key returns the Definition key that owns this instance.
func (i *Instance) Key() plugin.Key { return i.key }

// ID returns the dependency-graph node id: "gorm" for the default instance
// and "gorm[readonly]" for a named instance.
func (i *Instance) ID() string {
	if i.instance == defaultInstance {
		return i.key.String()
	}
	return i.key.String() + "[" + i.instance + "]"
}

// Label is the diagnostic form. A multiple-instance definition displays its
// default instance explicitly so it cannot be mistaken for a single one.
func (i *Instance) Label() string {
	if i.instance != defaultInstance {
		return i.ID()
	}
	if i.multiple {
		return i.key.String() + "[" + defaultInstance + "]"
	}
	return i.key.String()
}

// Plugin returns the live plugin value this Instance wraps.
func (i *Instance) Plugin() plugin.Plugin { return i.plugin }

// Context returns the one Context bound during expansion.
func (i *Instance) Context() *plugin.Context { return i.ctx }

// Deps returns a defensive copy of the merged dependency declaration.
func (i *Instance) Deps() plugin.Deps {
	return plugin.Deps{
		Types:   append([]plugin.Dep(nil), i.deps.Types...),
		Plugins: append([]plugin.Ref(nil), i.deps.Plugins...),
		After:   append([]plugin.Key(nil), i.deps.After...),
		Before:  append([]plugin.Key(nil), i.deps.Before...),
	}
}

// Provides returns a defensive copy of the merged product declarations.
func (i *Instance) Provides() []plugin.Dep {
	return append([]plugin.Dep(nil), i.provides...)
}

func (i *Instance) configPath() string {
	if i.multiple {
		return "plugins." + i.key.String() + "." + i.instance
	}
	return "plugins." + i.key.String()
}
