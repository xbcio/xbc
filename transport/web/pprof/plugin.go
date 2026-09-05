package pprof

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and runtime identity.
const Key plugin.Key = "pprof"

// Plugin contributes self-protected pprof routes.
type Plugin struct {
	settings settings
}

var _ web.RouteContributor = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable plugin with diagnostics disabled.
func New() *Plugin {
	value, _ := newPlugin(defaultConfig())
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if _, err := normalizeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{settings: normalized}, nil
}

func (p *Plugin) currentSettings() settings { return p.settings }

// Definition returns pprof's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns pprof's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
