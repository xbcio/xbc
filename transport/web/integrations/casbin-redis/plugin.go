package casbinredis

import (
	"github.com/xbcio/xbc/plugin"
	xbccasbin "github.com/xbcio/xbc/transport/web/integrations/casbin"
)

// Key is this plugin's stable configuration and dependency identity.
const Key plugin.Key = "casbin-redis"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Factory, error) {
		return New(cfg)
	},
	plugin.Options[*Factory]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[xbccasbin.WatcherFactory](func(value *Factory) xbccasbin.WatcherFactory { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns this integration's canonical immutable declaration.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return bundle }
