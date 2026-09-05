package securityheaders

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/cors"
	"github.com/xbcio/xbc/transport/web/ratelimit"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "securityheaders"

// Plugin contributes browser security headers to transport/web.
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

// New constructs directly usable security-header middleware with safe defaults.
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

// Order runs security headers outside optional same-phase middleware that can
// short-circuit, so preflight and rejected responses receive the headers too.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseSecurity,
		Before: []web.OrderRef{
			web.Prefer(cors.Key),
			web.Prefer(ratelimit.Key),
		},
	}
}

// Definition returns securityheaders' canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns securityheaders' side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
