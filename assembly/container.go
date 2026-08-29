// Package assembly turns frozen plugin definitions into configured,
// dependency-ordered live instances and wires their provided values. It is the
// lower-level public API for hosts that need custom assembly; package runtime
// remains responsible for lifecycle orchestration and shutdown.
package assembly

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
	"github.com/xbcio/xbc/plugin/ordering"
)

// Options is everything New needs to build a Container.
type Options struct {
	// Snapshot is the frozen set of plugin Definitions this Container
	// expands. It is immutable and safe to reuse across several Containers
	// (e.g. one per test case) -- see registry.go's doc comment on
	// valueRegistry for the corresponding independence guarantee on the
	// value-store side.
	Snapshot catalog.Snapshot

	// Env is the merged configuration environment passed to every expanded
	// instance's *plugin.Context through the read-only config.View interface;
	// Configurable instances bind against the concrete Environment here.
	Env *config.Environment

	// Host is the plugin.RuntimeHost every expanded instance's *plugin.Context is
	// built around. The caller (package runtime's hostAdapter) is expected
	// to forward ProvideValue/LookupValue/InitializedPlugins straight
	// through to this same Container, and GoManaged to its own managed task
	// runtime -- Container has no goroutine-lifecycle concerns of its own.
	Host plugin.RuntimeHost

	// Logger is the base logger newInstance derives each instance's own
	// "plugin"/"instance"-tagged child logger from.
	Logger log.Logger
}

// Container turns a frozen catalog.Snapshot into live, dependency-ordered
// plugin instances. It owns the assembly pipeline (expand -> bind config ->
// resolve) and the (type, instance) value registry every instance's Context
// reads and writes through, but none of the rollback/shutdown machinery that
// consumes its output -- that stays package runtime's job (see its
// shutdown.go unwind and stopBounded functions).
type Container struct {
	snapshot catalog.Snapshot
	env      *config.Environment
	host     plugin.RuntimeHost
	logger   log.Logger

	registry *valueRegistry

	order      []*Instance
	disabled   []string
	softMisses []ordering.Miss

	mu          sync.Mutex
	initialized []*Instance
	sealed      bool
}

// New builds a Container. It does not run the assembly pipeline itself --
// call Assemble for that -- so a Container that fails to Assemble is still a
// perfectly valid, inspectable value (e.g. for a test that only wants to
// check the error).
func New(opts Options) *Container {
	return &Container{
		snapshot: opts.Snapshot,
		env:      opts.Env,
		host:     opts.Host,
		logger:   opts.Logger,
		registry: newValueRegistry(),
	}
}

// Assemble owns the complete plugin assembly sequence: expand the
// Snapshot into Instances, bind and validate each Configurable instance's
// config section, then resolve the dependency graph into a topological Order.
// It is not
// safe to call twice on the same Container -- a second call would re-expand
// against a Snapshot that may have already been mutated by the first call's
// side effects (Factory calls have already run) with no defined semantics
// for what "assembling again" should mean.
func (c *Container) Assemble() error {
	insts, disabled, err := c.expand()
	if err != nil {
		return err
	}
	if err := c.bindConfigs(insts); err != nil {
		return err
	}
	order, misses, err := c.resolve(insts)
	if err != nil {
		return err
	}
	c.order = order
	c.disabled = disabled
	c.softMisses = misses
	return nil
}

// Order returns every expanded instance in topological order, as a fresh
// defensive slice on every call.
func (c *Container) Order() []*Instance {
	out := make([]*Instance, len(c.order))
	copy(out, c.order)
	return out
}

// Disabled returns the keys of every Definition in the Snapshot that
// expanded into zero instances -- because its Activation evaluated to
// disabled, an explicit `enabled: false`, or (for a multiple-instance
// Definition) a config section that declared no enabled instance. The result
// is sorted and a
// fresh defensive slice on every call.
func (c *Container) Disabled() []string {
	out := make([]string, len(c.disabled))
	copy(out, c.disabled)
	return out
}

// SoftMisses returns every soft After/Before ordering constraint whose
// referenced Definition key was never expanded into an instance. Never fatal
// -- a preference that points at an absent key is stale, not a
// broken dependency.
func (c *Container) SoftMisses() []ordering.Miss {
	out := make([]ordering.Miss, len(c.softMisses))
	copy(out, c.softMisses)
	return out
}

// MarkInitialized records that inst has completed its lifecycle
// initialization (Inject -> Init -> Harvest, driven by the caller). It is
// the caller's responsibility to call this in the same topological order
// Order() returned -- Container trusts that discipline rather than
// re-sorting, exactly as the old registry trusted its own callers' call
// order for diagnostics.
func (c *Container) MarkInitialized(inst *Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized = append(c.initialized, inst)
}

// Initialized returns every instance MarkInitialized has recorded so far,
// in the order they were marked, as a fresh defensive slice.
func (c *Container) Initialized() []*Instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Instance, len(c.initialized))
	copy(out, c.initialized)
	return out
}

// SealInitialization marks that every instance has finished initializing.
// Only after this has been called does InitializedPlugins stop erroring.
func (c *Container) SealInitialization() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealed = true
}

// ProvideValue stores v under (typ, instance) in the Container's value
// registry. This is the plugin.RuntimeHost method package runtime's hostAdapter
// forwards straight through to, backing plugin.Provide[T]/Get[T]/
// GetNamed[T] for every Context built by this Container.
func (c *Container) ProvideValue(typ reflect.Type, instance string, v any) {
	c.registry.put(typ, instance, v)
}

// LookupValue looks up (typ, instance) in the Container's value registry.
// See ProvideValue's doc comment for how this fits into plugin.RuntimeHost.
func (c *Container) LookupValue(typ reflect.Type, instance string) (any, error) {
	return c.registry.lookup(typ, instance)
}

// InitializedPlugins returns every marked-initialized instance as a
// plugin.Extension[any], in the topological order they were marked, once
// SealInitialization has been called. Calling this before sealing is an
// error -- it must never hand back a partial snapshot dressed up as a
// complete one, since that is exactly the invariant plugin.Extensions[T]
// depends on to be safe to call from any plugin's Init.
func (c *Container) InitializedPlugins() ([]plugin.Extension[any], error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.sealed {
		return nil, fmt.Errorf("xbc: not all plugins initialized yet, cannot query InitializedPlugins")
	}

	out := make([]plugin.Extension[any], len(c.initialized))
	for i, inst := range c.initialized {
		out[i] = plugin.Extension[any]{Identity: inst.Identity(), Value: inst.plugin}
	}
	return out, nil
}
