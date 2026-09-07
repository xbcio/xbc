package gracefulshutdown

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const (
	// Key is the stable identity of the programmatic shutdown Controller.
	Key plugin.Key = "gracefulshutdown"
	// HTTPKey identifies the independently constructed HTTP route contributor.
	HTTPKey plugin.Key = "gracefulshutdown-http"

	configPath = "plugins.gracefulshutdown"
)

// Plugin is the optional HTTP adapter for Controller. It has a separate
// Definition because the route contributor and programmatic controller are
// independently selected products.
type Plugin struct {
	endpoint   endpointConfig
	controller *Controller
}

var _ web.RouteContributor = (*Plugin)(nil)

var controllerInput = plugin.RefTo[*Controller](Key)

var controllerDefinition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Controller, error) { return New(), nil },
	plugin.Options[*Controller]{
		Activation: plugin.WhenConfigured(configPath),
	},
)

var httpDefinition = plugin.DefineConfigured(
	HTTPKey,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, controllerInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured(configPath),
		ConfigPath: configPath,
		Inputs:     plugin.Inputs(controllerInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(controllerDefinition, httpDefinition)

func prepareConfig(cfg Config) (Config, error) {
	if _, err := normalizeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, controller *Controller) (*Plugin, error) {
	endpoint, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{endpoint: endpoint, controller: controller}, nil
}

// Definition returns the canonical programmatic Controller declaration.
func Definition() plugin.Definition { return controllerDefinition }

// Bundle returns both the Controller and HTTP route-contributor Definitions.
func Bundle() plugin.Bundle { return bundle }
