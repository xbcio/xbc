package cors

import (
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "cors"

// Plugin contributes the CORS middleware to transport/web.
type Plugin struct {
	mu     sync.RWMutex
	policy *policy
}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return New(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable CORS middleware from cfg.
func New(cfg Config) (*Plugin, error) {
	compiled, err := compilePolicy(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{policy: compiled}, nil
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.handle) }

// Order places CORS in the request-security phase.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseSecurity}
}

// Definition returns CORS's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns CORS's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
