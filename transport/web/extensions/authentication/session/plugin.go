package session

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is this plugin's stable configuration, dependency, and Definition
// identity.
const Key plugin.Key = "session"

// redisPluginKey mirrors the stable plugin.Key identity owned by the optional
// extensions/storage/redis plugin. session must not import that sibling
// integration module (each optional integration is independently
// versioned), so the identity is duplicated here as a typed constant.
const redisPluginKey plugin.Key = "redis"

// Plugin owns session persistence and contributes a credential extractor and
// an authenticator to the Web transport's authentication middleware. It
// embeds *manager so it satisfies Manager directly through promoted methods,
// which plugin.ExportAs requires of a Definition's primary type.
type Plugin struct {
	config normalizedConfig
	*manager
	owned *MemoryStore
}

type options struct {
	store Store
}

// Option customizes a programmatically assembled plugin.
type Option func(*options)

// WithStore injects a Store and bypasses configured backend construction. Its
// lifecycle remains caller-owned.
func WithStore(store Store) Option { return func(o *options) { o.store = store } }

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
			plugin.ExportAs[authentication.Authenticator](func(value *Plugin) authentication.Authenticator { return value }),
			plugin.ExportAs[web.CredentialExtractor](func(value *Plugin) web.CredentialExtractor { return value }),
			plugin.ExportAs(func(value *Plugin) Manager { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns session's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns session's side-effect-free explicit composition bundle.
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
		return plugin.PlanOf(plugin.Inputs(), func(plugin.BuildContext) (*Plugin, error) {
			return newMemoryPlugin(config)
		}), nil
	}

	client := plugin.RefToInstance[*goredis.Client](redisPluginKey, config.redisInstance)
	return plugin.PlanOf(plugin.Inputs(client), func(context plugin.BuildContext) (*Plugin, error) {
		value := client.Get(context).Value
		if value == nil {
			return nil, fmt.Errorf("session: Redis client instance %q is unavailable", config.redisInstance)
		}
		store, err := NewRedisStore(value, config.redisPrefix)
		if err != nil {
			return nil, err
		}
		return newPlugin(config, store, nil), nil
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
	if built.store != nil {
		return newPlugin(config, built.store, nil), nil
	}
	if config.backend != BackendMemory {
		return nil, fmt.Errorf("session: backend %q requires WithStore(...) for direct construction", config.backend)
	}
	return newMemoryPlugin(config)
}

func newMemoryPlugin(config normalizedConfig) (*Plugin, error) {
	owned, err := NewMemoryStore(config.cleanupInterval)
	if err != nil {
		return nil, err
	}
	return newPlugin(config, owned, owned), nil
}

func newPlugin(config normalizedConfig, store Store, owned *MemoryStore) *Plugin {
	return &Plugin{
		config:  config,
		manager: &manager{store: store, cfg: config},
		owned:   owned,
	}
}

// Manager returns this plugin's handler-facing session API.
func (p *Plugin) Manager() Manager { return p.manager }

// Stop revokes process-local sessions and stops memory cleanup. Injected and
// Redis stores are externally owned. Repeated and concurrent calls are safe
// because manager.stop and MemoryStore.Close are independently idempotent.
func (p *Plugin) Stop(context.Context) error {
	p.manager.stop()
	if p.owned != nil {
		return p.owned.Close()
	}
	return nil
}
