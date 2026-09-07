package tenant

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "tenant"

type runtimeState struct {
	cfg      normalizedConfig
	resolver Resolver
}

// Plugin contributes verified tenant resolution after authentication.
type Plugin struct {
	cfg      Config
	resolver Resolver
	initMu   sync.Mutex
	state    atomic.Pointer[runtimeState]
}

// Option customizes a directly constructed Plugin.
type Option func(*Plugin)

// WithResolver injects trusted membership resolution. The resolver receives
// any header value only as an untrusted requestedID selector.
func WithResolver(resolver Resolver) Option { return func(p *Plugin) { p.resolver = resolver } }

var (
	_ plugin.Initializer = (*Plugin)(nil)
	_ web.Middleware     = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg), nil
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New returns a directly usable plugin with safe principal attribute defaults.
// The runtime state used to serve requests is published only after Init
// validates configuration and freezes a Resolver.
func New(options ...Option) *Plugin { return newPlugin(defaultConfig(), options...) }

func newPlugin(cfg Config, options ...Option) *Plugin {
	p := &Plugin{cfg: cfg}
	for _, option := range options {
		if option != nil {
			option(p)
		}
	}
	return p
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Definition returns tenant's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns tenant's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Init validates configuration and freezes the trusted Resolver snapshot. It
// is safe to call concurrently; only the first successful call publishes
// runtime state, and every later call fails.
func (p *Plugin) Init(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("tenant: Init requires a non-nil plugin context")
	}
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.state.Load() != nil {
		return errors.New("tenant: plugin is already initialized")
	}
	cfg, err := normalizeConfig(p.cfg)
	if err != nil {
		return err
	}
	resolver := p.resolver
	if resolver == nil {
		resolver = principalResolver{cfg: cfg}
	}
	p.state.Store(&runtimeState{cfg: cfg, resolver: resolver})
	return nil
}
