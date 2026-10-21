package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"

	"github.com/xbcio/xbc/plugin"
)

// Key is this plugin's stable configuration and dependency identity. It owns
// the plugins.metrics configuration section; the Web adapter's HTTP middleware
// is a separate Definition under its own key, http-metrics.
const Key plugin.Key = "metrics"

// Plugin owns the application's private collector registry, built from
// validated configuration when the Plugin is constructed. It is the primary
// value the Definition constructs, so it is also the value the Registerer
// contract is satisfied by: the framework requires a contract's interface to
// be assignable from that primary type.
type Plugin struct {
	config   normalizedConfig
	registry *Registry
}

var _ Registerer = (*Plugin)(nil)
var _ Gatherer = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(ctx.Service(), cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[Registerer](func(value *Plugin) Registerer { return value }),
			plugin.ExportAs[Gatherer](func(value *Plugin) Gatherer { return value }),
		),
		// No Lifecycle adapter: the plugin owns no resource that outlives the
		// process beyond memory, so it has no Start, Drain, or Stop stage.
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns metrics' canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns metrics' side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// New constructs a directly usable Plugin from cfg: the configuration is
// validated and the private registry is created immediately, and a violation
// is returned rather than deferred to a later stage. It performs no
// process-wide registration.
//
// It registers the build_info series under an empty service identity, the
// same labels an application assembled through Bundle gets minus the identity
// the framework supplies; New exists for callers embedding the plugin
// directly.
func New(cfg Config) (*Plugin, error) {
	return newPlugin(plugin.ServiceIdentity{}, cfg)
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(service plugin.ServiceIdentity, cfg Config) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	registry := newRegistry()
	if err := registerBuildInfo(registry, normalized.namespace, service); err != nil {
		return nil, err
	}
	// The standard runtime collectors keep their canonical go_ and process_
	// names -- no namespace or subsystem prefix -- and carry no service labels:
	// they describe this process, not the service identity, which stays on
	// build_info and the business series.
	if normalized.goCollector {
		if err := registry.Register(collectors.NewGoCollector()); err != nil {
			return nil, fmt.Errorf("metrics: register Go runtime collector: %w", err)
		}
	}
	if normalized.processCollector {
		if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
			return nil, fmt.Errorf("metrics: register process collector: %w", err)
		}
	}
	return &Plugin{config: normalized, registry: registry}, nil
}

// Registry returns the Plugin's private collector registry.
func (p *Plugin) Registry() *Registry {
	if p == nil {
		return nil
	}
	return p.registry
}

// Register implements Registerer over the private registry.
func (p *Plugin) Register(collectors ...prometheus.Collector) error {
	return p.Registry().Register(collectors...)
}

// Unregister implements Registerer over the private registry.
func (p *Plugin) Unregister(collector prometheus.Collector) bool {
	return p.Registry().Unregister(collector)
}

// Gather implements Gatherer over the private registry. A zero Plugin reports
// an uninitialized registry rather than panicking, matching Registry.Gather.
func (p *Plugin) Gather() ([]*dto.MetricFamily, error) {
	return p.Registry().Gather()
}

// Namespace returns the validated namespace prefix this application's metric
// names are built with. The transport adapters apply it to the instruments
// they own rather than owning a configuration section of their own; a zero
// Plugin reports the empty string.
func (p *Plugin) Namespace() string {
	if p == nil {
		return ""
	}
	return p.config.namespace
}

// Subsystem returns the validated subsystem component of this application's
// metric names, empty when none was configured. The transport adapters apply
// it to the instruments they own.
func (p *Plugin) Subsystem() string {
	if p == nil {
		return ""
	}
	return p.config.subsystem
}

// DurationBuckets returns a copy of the configured histogram bucket bounds for
// duration instruments, so a caller cannot mutate the plugin's configuration.
func (p *Plugin) DurationBuckets() []float64 {
	if p == nil {
		return nil
	}
	return append([]float64(nil), p.config.buckets...)
}

// HTTP returns the normalized exposition endpoint settings from
// plugins.metrics.http: whether the endpoint is enabled and the cleaned static
// path it serves. The Web adapter owns the route and serves it; a zero Plugin
// reports a disabled endpoint.
func (p *Plugin) HTTP() HTTPConfig {
	if p == nil {
		return HTTPConfig{}
	}
	return HTTPConfig{Enabled: p.config.endpoint.enabled, Path: p.config.endpoint.path}
}
