package recovery

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "recovery"

type runtimeState struct {
	stack  bool
	logger corelog.Logger
}

// Plugin contributes the outermost HTTP panic boundary.
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
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, ctx.Log()), nil
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable recovery middleware with safe defaults.
func New() *Plugin { return newPlugin(DefaultConfig(), corelog.L()) }

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, logger corelog.Logger) *Plugin {
	if logger == nil {
		logger = corelog.L()
	}
	value := &Plugin{}
	value.state.Store(&runtimeState{stack: cfg.Stack, logger: logger})
	return value
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.handle) }

// Order places recovery outside every normal Web middleware phase.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseRecover}
}

// Definition returns recovery's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns recovery's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
