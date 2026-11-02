package redissyncmap

import (
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the Redis-backed
// syncmap plugin.
const Key plugin.Key = "redis-syncmap"

// redisPluginKey names the storage/redis plugin whose instance this integration
// binds. It is spelled here instead of imported: a capability plugin may not
// import a sibling capability module, and only the exported UniversalClient
// contract crosses the edge.
const redisPluginKey plugin.Key = "redis"

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	planFactory,
	plugin.Options[*factory]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.redis-syncmap"),
		Exports: plugin.Contracts(
			plugin.ExportAs[syncmap.Factory](func(value *factory) syncmap.Factory { return value }),
		),
		Lifecycle: plugin.Lifecycle[*factory]{
			Init:  initFactory,
			Drain: drainFactory,
			Stop:  stopFactory,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns this integration's canonical Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return bundle }

// planFactory binds the exact Redis client the prepared configuration names.
// It is pure: no connection is opened and no map is created here. Maps are
// created by consumers between construction and Start, which is the window in
// which a map can still be adopted as a managed subscription task.
func planFactory(config Config) (plugin.Plan[*factory], error) {
	client := plugin.RefToInstance[goredis.UniversalClient](redisPluginKey, config.RedisInstance)
	return plugin.PlanOf(plugin.Inputs(client), func(ctx plugin.BuildContext) (*factory, error) {
		return newFactory(ctx.Log(), client.Get(ctx).Value, config)
	}), nil
}
