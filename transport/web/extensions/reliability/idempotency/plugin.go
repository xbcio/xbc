package idempotency

import (
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is this plugin's stable configuration and dependency identity.
const Key plugin.Key = "idempotency"

// redisPluginKey mirrors the stable plugin.Key identity owned by the optional
// extensions/storage/redis plugin. idempotency must not import that sibling
// integration module (each optional integration is independently versioned),
// so the identity is duplicated here as a typed constant.
const redisPluginKey plugin.Key = "redis"

type runtimeState struct {
	config normalizedConfig
	store  Store
	logger log.Logger
}

// Plugin contributes route-aware duplicate suppression.
type Plugin struct {
	state *runtimeState
}

type options struct {
	store Store
}

// Option customizes a programmatically assembled plugin.
type Option func(*options)

// WithStore injects a Store and bypasses configured backend construction.
func WithStore(store Store) Option { return func(o *options) { o.store = store } }

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the side-effect-free canonical definition.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free canonical composition.
func Bundle() plugin.Bundle { return bundle }

// plan selects the Store's inputs from the prepared config: the memory
// backend needs none, while the redis backend requires the named
// *goredis.Client instance the configuration selects. The planner itself
// acquires no resource; the returned Plan's factory does that once the graph
// has wired the declared inputs.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}

	if config.backend == BackendMemory {
		return plugin.PlanOf(plugin.Inputs(), func(context plugin.BuildContext) (*Plugin, error) {
			return newPlugin(config, NewMemoryStore(), context.Log()), nil
		}), nil
	}

	client := plugin.RefToInstance[*goredis.Client](redisPluginKey, config.redisInstance)
	return plugin.PlanOf(plugin.Inputs(client), func(context plugin.BuildContext) (*Plugin, error) {
		value := client.Get(context).Value
		if value == nil {
			return nil, fmt.Errorf("idempotency: Redis client instance %q is unavailable", config.redisInstance)
		}
		store, err := NewRedisStore(value, config.redisPrefix)
		if err != nil {
			return nil, err
		}
		return newPlugin(config, store, context.Log()), nil
	}), nil
}

// New constructs a plugin from an explicit configuration. The redis backend
// requires WithStore for direct construction outside the Definition-driven
// composition, which resolves a named Redis client from the graph instead.
func New(cfg Config, opts ...Option) (*Plugin, error) {
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	config, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	store := built.store
	if store == nil {
		if config.backend != BackendMemory {
			return nil, fmt.Errorf("idempotency: backend %q requires WithStore(...) for direct construction", config.backend)
		}
		store = NewMemoryStore()
	}
	return newPlugin(config, store, log.Nop()), nil
}

func newPlugin(config normalizedConfig, store Store, logger log.Logger) *Plugin {
	return &Plugin{state: &runtimeState{config: config, store: store, logger: logger}}
}
