package gzip

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/timeout"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "gzip"

// Plugin contributes negotiated response compression to transport/web.
type Plugin struct {
	state atomic.Pointer[normalizedConfig]
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

// New constructs directly usable compression middleware with production
// defaults.
func New() *Plugin {
	state, _ := normalizeConfig(DefaultConfig())
	value := &Plugin{}
	value.state.Store(&state)
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config) (*Plugin, error) {
	state, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	value := &Plugin{}
	value.state.Store(&state)
	return value, nil
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return p.handle }

// Order runs compression inside timeout's complete-response buffer when that
// optional middleware is present.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseBusiness,
		After: []web.OrderRef{web.Prefer(timeout.Key)},
	}
}

// Definition returns gzip's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns gzip's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
