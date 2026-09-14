package ratelimit

import (
	"sync"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/security/cors"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "ratelimit"

// Plugin contributes request rate limiting to transport/web.
type Plugin struct {
	mu    sync.RWMutex
	state *limiterState
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

// New constructs directly usable rate-limiting middleware from cfg.
func New(cfg Config) (*Plugin, error) {
	state, err := newLimiterState(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{state: state}, nil
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Handler returns the middleware handler.
func (p *Plugin) Handler() web.Handler { return p.handle }

// Order lets optional CORS handling short-circuit preflight requests before
// they consume rate-limit capacity.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseSecurity,
		After: []web.OrderRef{web.Prefer(cors.Key)},
	}
}

// Definition returns ratelimit's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns ratelimit's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
