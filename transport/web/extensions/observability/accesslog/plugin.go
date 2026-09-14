package accesslog

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "accesslog"

type runtimeState struct {
	config normalizedConfig
	logger corelog.Logger
}

// Plugin contributes structured HTTP access logging.
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
		return newPlugin(cfg, ctx.Log())
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable access logging middleware with safe defaults.
func New() *Plugin {
	cfg, _ := normalizeConfig(DefaultConfig())
	value := &Plugin{}
	value.state.Store(&runtimeState{config: cfg, logger: corelog.L()})
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, logger corelog.Logger) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = corelog.L()
	}
	value := &Plugin{}
	value.state.Store(&runtimeState{config: normalized, logger: logger})
	return value, nil
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.handle) }

// Order places access logging in the observation phase. The optional
// request-ID relationship is declared by requestid to avoid duplicate edges.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseObserve}
}

// Definition returns accesslog's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns accesslog's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
