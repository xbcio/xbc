package casbin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition and configuration identity of the HTTP route
// authorization middleware. It is distinct from the neutral engine's own key
// (casbin) because the two are independently selected products with separate
// configuration sections: a non-Web service composes the engine without any
// HTTP surface.
const Key plugin.Key = "casbin-http"

// Option customizes a directly constructed Plugin.
type Option func(*options)

type options struct {
	resolver SubjectResolver
}

// WithSubjectResolver replaces the default web.CurrentPrincipal resolver.
// The supplied resolver is responsible for accepting only verified identity
// facts. A nil resolver falls back to the default Principal contract.
func WithSubjectResolver(resolver SubjectResolver) Option {
	return func(o *options) { o.resolver = resolver }
}

type runtimeState struct {
	cfg      normalizedConfig
	provider casbincore.EnforcerProvider
	resolver SubjectResolver

	// convention is the request tuple this middleware sends, taken once from
	// the engine's EnforcerProvider at construction. It is deliberately never
	// re-read from the enforcer: SyncedEnforcer.GetModel returns the model
	// field LoadPolicy replaces under its own lock, so a model read would race
	// the engine's periodic reload, a watcher notification, or an explicit
	// LoadPolicy.
	convention casbincore.RequestConvention

	// logger is populated once by start, before traffic opens. It is never
	// retained as a *plugin.Context: only this derived, safe-to-share value is
	// kept.
	logger corelog.Logger
}

// Plugin enforces Casbin policy on matched Web routes. It owns no enforcer:
// the engine is a separate plugin whose EnforcerProvider this middleware
// consumes, so route enforcement can be added to a service, or left out of
// one, without moving the policy backend or its lifecycle with it.
type Plugin struct {
	state *runtimeState

	started atomic.Bool
	stopped atomic.Bool
}

var _ web.Middleware = (*Plugin)(nil)

// providerInput is the exact engine capability this middleware requires:
// the default instance of the plugin keyed casbin, read through its
// EnforcerProvider contract rather than its concrete primary type.
var providerInput = plugin.RefTo[casbincore.EnforcerProvider](casbincore.Key)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Start: (*Plugin).start,
		},
	},
)

// bundle selects the neutral engine alongside the middleware. Enforcing
// routes without an enforcer behind it is not a composition anyone wants, and
// RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(casbincore.Bundle(), plugin.BundleOf(definition))

// plan declares the exact engine capability this middleware consumes. It is
// pure and acquires nothing.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}
	return plugin.PlanOf(plugin.Inputs(providerInput), func(ctx plugin.BuildContext) (*Plugin, error) {
		provider := providerInput.Get(ctx).Value
		if isNil(provider) {
			return nil, fmt.Errorf("casbin-http: engine %s returned a nil enforcer provider", casbincore.Key)
		}
		return newPlugin(provider, normalized, nil)
	}), nil
}

// New constructs a directly usable middleware around an existing enforcer
// provider. Inline and file policies, adapters, and watchers belong to the
// engine and are not configured here.
func New(provider casbincore.EnforcerProvider, cfg Config, opts ...Option) (*Plugin, error) {
	if isNil(provider) {
		return nil, errors.New("casbin-http: requires an enforcer provider")
	}
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newPlugin(provider, normalized, built.resolver)
}

// newPlugin takes the request convention once, at construction, from the
// provider that also validates it, and refuses to build a middleware that
// cannot be enforced. The engine refuses its own models that disagree with the
// convention it reports, so the two cannot contradict each other; a foreign
// EnforcerProvider carries the same obligation, which is why the reported
// value is checked against the conventions this middleware can evaluate. The
// enforcer itself is not owned or retained here: the middleware looks the live
// one up through the provider on every request, because the engine's Stop can
// end it independently.
func newPlugin(provider casbincore.EnforcerProvider, cfg normalizedConfig, resolver SubjectResolver) (*Plugin, error) {
	if enforcer, active := provider.Enforcer(); !active || enforcer == nil {
		return nil, errors.New("casbin-http: enforcer provider reported no active enforcer; the middleware cannot be built against a stopped engine")
	}
	convention := provider.RequestConvention()
	if convention != casbincore.ConventionRoutePermission && convention != casbincore.ConventionPathMethod {
		return nil, fmt.Errorf("casbin-http: enforcer provider declares unsupported request convention %q; supported conventions are %q and %q",
			convention, casbincore.ConventionRoutePermission, casbincore.ConventionPathMethod)
	}
	if resolver == nil {
		resolver = principalSubjectResolver{}
	}
	return &Plugin{
		state: &runtimeState{
			cfg:        cfg,
			provider:   provider,
			resolver:   resolver,
			convention: convention,
			logger:     corelog.Nop(),
		},
	}, nil
}

// isNil reports whether the capability a build step received is absent,
// including the typed-nil case a plain interface comparison misses.
func isNil(value any) bool {
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

// Definition returns the middleware's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the middleware's side-effect-free explicit composition
// bundle: the engine and the middleware that consumes it.
func Bundle() plugin.Bundle { return bundle }

// start records the runtime logger. It stores only values derived from ctx
// and never retains the *plugin.Context itself.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("casbin-http: Start requires a non-nil plugin context")
	}
	if p.stopped.Load() {
		return errors.New("casbin-http: plugin is stopped")
	}
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("casbin-http: Start called more than once")
	}
	p.state.logger = ctx.Log()
	return nil
}

// Stop marks the middleware inactive so every matched route fails closed from
// the first request after shutdown begins. It deliberately does not stop the
// engine: the enforcer provider is a separate plugin that other consumers --
// rbac.Backend callers, non-Web services -- may still be using, and only its
// own Stop owns the enforcer.
func (p *Plugin) Stop(context.Context) error {
	p.stopped.Store(true)
	return nil
}
