// host.go
package plugin

import (
	"context"
	"reflect"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
)

// RuntimeHost is the framework-implemented, plugin-package-consumed narrow port
// that connects plugin.Context back to the container and task runtime,
// without plugin importing the root package or internal/container (Go's
// package boundaries would otherwise force one of them to). It has exactly
// one real implementation -- the root package's hostAdapter -- and it is
// not an extension point for plugin authors: ordinary plugins never see a
// RuntimeHost value directly, only the Context built around one.
//
//	plugin.Context ──► plugin.RuntimeHost ◄── runtime hostAdapter ──► container / task runtime
//
// RuntimeHost carries only lookup/provide/extensions/task mechanics. It must never
// grow a Router, a Gin or gRPC type, or a full *App -- those would make
// plugin protocol-aware or give ordinary plugins a backdoor to the whole
// host, exactly what routing this through a narrow port is meant to avoid.
//
// The method set below is pinned deliberately -- four methods, no fifth.
// Adding a method requires proving it cannot be built by composing the
// existing four, and recording that proof here; "this would be more
// convenient" is not a justification. InitializedPlugins is the widest of
// the four, and still counts as narrow: narrowness is not about how many
// objects a method returns, it is about how much structure each returned
// object exposes. InitializedPlugins returns Extension[any].Value, which is
// the plugin instance itself (a plugin.Plugin dynamic value); plugin only
// ever does one thing with it -- a reflect assignability check -- and never
// sees the instance's Context, bound config, Deps, provided values, init
// state, or any other internal/container.Instance field. The container's
// internal shape therefore never enters plugin's API surface, and
// Instance never needs to be exported.
type RuntimeHost interface {
	// ProvideValue and LookupValue are the storage behind Provide[T] /
	// Get[T] / GetNamed[T] (registry.go): a (type, instance) keyed value
	// store shared by every Context belonging to the same App.
	ProvideValue(typ reflect.Type, instance string, v any)
	LookupValue(typ reflect.Type, instance string) (any, error)

	// InitializedPlugins is the storage behind Extensions[T]
	// (extension.go). It returns every enabled, fully-Init'd plugin
	// instance, in core's dependency-topological order, as a fresh
	// defensive slice on every call. Calling it before every plugin has
	// finished Init must return an error -- never a partial snapshot.
	InitializedPlugins() ([]Extension[any], error)

	// GoManaged is the storage behind Context.Go / Context.GoCritical
	// (context.go): hand fn to the host's managed task group, tagged with
	// the owning plugin's Identity and whether an unprompted return or
	// panic should be treated as a critical failure.
	GoManaged(id Identity, fn func(context.Context), critical bool)
}

// NewRuntimeContext builds a *Context wired to host, under the given
// Definition-key/instance Identity, config.View and logger. It is the
// assembly-time entry point -- called by internal/container while
// expanding a Definition into a live instance -- not a constructor ordinary
// plugin code should ever call: a plugin receives its Context as an
// argument (Init(ctx *Context), Start(ctx *Context), ...), it never builds
// one.
func NewRuntimeContext(host RuntimeHost, id Identity, env config.View, logger log.Logger) *Context {
	return &Context{
		host:   host,
		id:     Identity{Plugin: id.Plugin, Instance: NormalizeInstance(id.Instance)},
		env:    env,
		logger: logger,
	}
}

// BindRuntimeContext wires ctx and the Definition key into p's embedded Base,
// if it has one. Like NewRuntimeContext above, this is a framework assembly-time
// entry point -- called by the container while expanding a Definition into a live instance, under
// the exact Definition.Key that Definition carries (rule 1, package-layout
// design §3.3: Definition.Key is a plugin's sole identity) -- never by
// ordinary plugin code, which receives its Context as a lifecycle-method
// argument and never needs to bind one itself.
//
// It returns false when p does not embed Base at all: that is a legal
// shape, not an error -- a plugin that writes Init(ctx *Context) itself
// already has everything Base would give it, via ctx directly, and simply
// has nothing here for BindRuntimeContext to wire up.
func BindRuntimeContext(p Plugin, ctx *Context, key Key) bool {
	return bindBase(p, ctx, key)
}
