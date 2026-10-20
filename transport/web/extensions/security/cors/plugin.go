package cors

import (
	"sync"

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

var (
	_ web.Middleware = (*Plugin)(nil)
	_ plugin.Runner  = (*Plugin)(nil)
)

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

// Start emits the one advisory warning a wildcard policy deserves: a service
// whose compiled allow_origins accepts every origin lets any browser origin
// that can reach it read its responses, which is easy to miss in a
// configuration that only says plugins.cors.enabled: true. The warning names
// the remedy and carries no request data. It is emitted once per process --
// Start runs once -- and never fails: an explicit origin list is silent, and
// CORS keeps working either way.
func (p *Plugin) Start(ctx *plugin.Context) error {
	if ctx == nil {
		return nil
	}
	p.mu.RLock()
	compiled := p.policy
	p.mu.RUnlock()
	if compiled == nil || !compiled.anyOrigin {
		return nil
	}
	ctx.Log().Warn("web: CORS allows every origin",
		"allow_credentials", compiled.allowCredentials,
		"remedy", "set plugins.cors.allow_origins to the browser origins allowed to call this service",
	)
	return nil
}

// Handler returns the middleware handler.
func (p *Plugin) Handler() web.Handler { return p.handle }

// Order places CORS in the request-security phase.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseSecurity}
}

// Definition returns CORS's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns CORS's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
