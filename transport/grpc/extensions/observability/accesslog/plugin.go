package accesslog

import (
	"sync/atomic"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Key is the stable Definition, configuration, and interceptor identity of
// gRPC access logging. It is read "grpc" first for the same reason as
// grpc-metrics: it keeps the plugin's section distinct from the Web access
// log's plugins.accesslog, so one process can configure both independently.
const Key plugin.Key = "grpc-accesslog"

type runtimeState struct {
	config normalizedConfig
	logger corelog.Logger
}

// Plugin contributes structured gRPC access logging. The same primary value
// contributes both the unary and the stream interceptor.
type Plugin struct {
	state atomic.Pointer[runtimeState]
}

var (
	_ grpctransport.UnaryInterceptor  = (*Plugin)(nil)
	_ grpctransport.StreamInterceptor = (*Plugin)(nil)
)

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
			plugin.ExportAs[grpctransport.UnaryInterceptor](func(value *Plugin) grpctransport.UnaryInterceptor { return value }),
			plugin.ExportAs[grpctransport.StreamInterceptor](func(value *Plugin) grpctransport.StreamInterceptor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable access logging interceptors with safe
// defaults, writing to the process logger.
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

// Definition returns the gRPC access log's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the gRPC access log's side-effect-free explicit composition
// bundle.
func Bundle() plugin.Bundle { return bundle }
