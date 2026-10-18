package oidc

import (
	"errors"
	"reflect"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	oidccore "github.com/xbcio/xbc/extensions/authentication/oidc"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

// Key is the stable configuration, dependency, and Definition identity of the
// OIDC Web adapter.
const Key plugin.Key = "oidc-http"

var (
	// loginInput requires exactly one shared federation login base. Missing or
	// duplicated implementations fail at plan time.
	loginInput = plugin.RequireOne[federationweb.Login]()
	// registryInput requires exactly one OIDC provider registry. The value is
	// the type oidc.NewRegistry returns; the application owns it, loads
	// providers into it at runtime, and exports it from its own plugin.
	registryInput = plugin.RequireOne[*federationcore.Registry[*oidccore.Provider]]()
)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		// The adapter exists only for a composition that selects it, exactly
		// like the shared base and every other optional Web extension.
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[transportweb.RouteContributor](func(value *Plugin) transportweb.RouteContributor { return value }),
		),
	},
)

// bundle selects the shared federation base, and with it the session plugins a
// completed login hands its session to, alongside the adapter. Enabling the
// adapter without the base would fail plan-time input resolution anyway; the
// combined bundle makes the intended composition explicit instead.
var bundle = plugin.CombineBundles(federationweb.Bundle(), plugin.BundleOf(definition))

// Definition returns the adapter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the adapter, the
// shared federation login base, and the session plugins the base requires.
func Bundle() plugin.Bundle { return bundle }

// Plugin is the OIDC Web adapter: two public routes that drive one
// authorization-code login through the neutral OIDC client and the shared
// federation login base. It starts no goroutine, owns no connection, and
// performs no asynchronous work, so it implements neither plugin.Drainer nor
// Stop.
type Plugin struct {
	config   normalizedConfig
	registry *federationcore.Registry[*oidccore.Provider]
	login    federationweb.Login
	logger   log.Logger
}

var _ transportweb.RouteContributor = (*Plugin)(nil)

type options struct {
	logger log.Logger
}

// Option customizes a directly constructed Plugin.
type Option func(*options)

// WithLogger injects the logger internal failure detail is written to.
func WithLogger(logger log.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// New constructs a Plugin directly, outside the Definition-driven composition.
//
// registry is the OIDC provider registry the application owns, normally the
// value oidc.NewRegistry returned. login is the shared federation base; the
// Definition resolves both from the graph instead.
func New(
	cfg Config,
	registry *federationcore.Registry[*oidccore.Provider],
	login federationweb.Login,
	opts ...Option,
) (*Plugin, error) {
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	config, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newPlugin(config, registry, login, built.logger)
}

// plan binds the two required inputs. The planner itself acquires no resource;
// the returned Plan's factory runs once the graph has wired those inputs.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}
	return plugin.PlanOf(plugin.Inputs(loginInput, registryInput), func(ctx plugin.BuildContext) (*Plugin, error) {
		return newPlugin(config, registryInput.Get(ctx).Value, loginInput.Get(ctx).Value, ctx.Log())
	}), nil
}

func newPlugin(
	config normalizedConfig,
	registry *federationcore.Registry[*oidccore.Provider],
	login federationweb.Login,
	logger log.Logger,
) (*Plugin, error) {
	if nilInterface(registry) {
		return nil, errors.New("oidc-http: requires an OIDC provider registry")
	}
	if nilInterface(login) {
		return nil, errors.New("oidc-http: requires the shared federation login base")
	}
	if logger == nil {
		logger = log.L()
	}
	return &Plugin{
		config:   config,
		registry: registry,
		login:    login,
		logger:   logger,
	}, nil
}

func (p *Plugin) log() log.Logger {
	if p == nil || p.logger == nil {
		return log.L()
	}
	return p.logger
}

// nilInterface reports whether value is nil or a typed-nil implementation
// stored in an interface slot.
func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
