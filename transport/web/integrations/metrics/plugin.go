package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and Plugin identity.
const Key plugin.Key = "metrics"

type runtimeState struct {
	config   normalizedConfig
	registry *Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
	handler  *promhttp.HandlerOpts
}

// Plugin owns HTTP instruments and one private Prometheus registry. The same
// primary value contributes both request instrumentation and the scrape route.
type Plugin struct {
	state atomic.Pointer[runtimeState]
}

var (
	_ web.Middleware       = (*Plugin)(nil)
	_ web.RouteContributor = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable Plugin with an isolated registry and safe
// defaults. It performs no process-wide registration.
func New() *Plugin {
	value, _ := newPlugin(DefaultConfig())
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	state, err := newRuntimeState(normalized)
	if err != nil {
		return nil, err
	}
	value := &Plugin{}
	value.state.Store(state)
	return value, nil
}

// Definition returns metrics' canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns metrics' side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Registry returns the Plugin's private collector registry.
func (p *Plugin) Registry() *Registry {
	if p == nil {
		return nil
	}
	state := p.state.Load()
	if state == nil {
		return nil
	}
	return state.registry
}

func newRuntimeState(cfg normalizedConfig) (*runtimeState, error) {
	registry := newRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: cfg.namespace,
		Subsystem: cfg.subsystem,
		Name:      "http_requests_total",
		Help:      "Total HTTP requests handled by route template and status class.",
	}, []string{"method", "route", "status_class"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: cfg.namespace,
		Subsystem: cfg.subsystem,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds by route template and status class.",
		Buckets:   append([]float64(nil), cfg.buckets...),
	}, []string{"method", "route", "status_class"})
	inflight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: cfg.namespace,
		Subsystem: cfg.subsystem,
		Name:      "http_requests_in_flight",
		Help:      "Current HTTP requests in flight by route template.",
	}, []string{"method", "route"})
	if err := registry.Register(requests, duration, inflight); err != nil {
		return nil, err
	}
	opts := promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError}
	return &runtimeState{
		config: cfg, registry: registry, requests: requests,
		duration: duration, inflight: inflight, handler: &opts,
	}, nil
}
