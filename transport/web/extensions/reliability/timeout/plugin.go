package timeout

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "timeout"

type runtimeState struct {
	config normalizedConfig
}

// Plugin contributes cooperative request deadlines to transport/web.
type Plugin struct {
	state atomic.Pointer[runtimeState]
}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable timeout middleware with production defaults.
func New() *Plugin {
	cfg, _ := normalizeConfig(DefaultConfig())
	value := &Plugin{}
	value.state.Store(&runtimeState{config: cfg})
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	value := &Plugin{}
	value.state.Store(&runtimeState{config: normalized})
	return value, nil
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.handle) }

// Order places the timeout buffer in the business phase. Gzip declares the
// optional edge that keeps compression inside this buffer.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseBusiness}
}

// Definition returns timeout's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns timeout's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
