package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition and middleware identity of the built-in HTTP
// instrumentation. It is distinct from the neutral metrics plugin's own key,
// metrics, which owns the plugins.metrics section: the instrumentation and its
// exposition endpoint have no configuration of their own and read their
// namespace, subsystem, duration buckets, and endpoint settings from that
// plugin.
//
// The name is deliberately read "http" first. A "-http" suffix would spell the
// same environment-variable tokens as the neutral section's own http sub-path:
// XBC_PLUGINS_METRICS_HTTP_ENABLED would then be ambiguous between the two
// kinds of section instead of configuring the endpoint.
const Key plugin.Key = "http-metrics"

// Plugin instruments HTTP traffic and serves the neutral metrics plugin's
// private registry on the optional exposition endpoint. The same primary value
// contributes both request instrumentation and the scrape route.
type Plugin struct {
	core     *metricscore.Plugin
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
	handler  promhttp.HandlerOpts
}

var (
	_ web.Middleware       = (*Plugin)(nil)
	_ web.RouteContributor = (*Plugin)(nil)
)

var metricsInput = plugin.RefTo[*metricscore.Plugin](metricscore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return New(metricsInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		// The HTTP instrumentation follows the metrics plugin it feeds:
		// configuring plugins.metrics turns both on, and leaving it out leaves
		// both dormant.
		Activation: plugin.WhenConfigured("plugins." + metricscore.Key.String()),
		Inputs:     plugin.Inputs(metricsInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

// bundle selects the neutral metrics plugin alongside the built-in HTTP
// instrumentation. The instruments register into that plugin's registry, and
// RefTo would reject the instrumentation without it at planning time anyway.
var bundle = plugin.CombineBundles(metricscore.Bundle(), plugin.BundleOf(definition))

// New builds the built-in HTTP instrumentation over the neutral metrics plugin
// and registers its three bounded-cardinality instruments with that plugin's
// registry, so one endpoint serves them beside application collectors.
func New(core *metricscore.Plugin) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("http-metrics: requires the metrics plugin")
	}
	registry := core.Registry()
	if registry == nil {
		return nil, errors.New("http-metrics: the metrics plugin has no registry")
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "http_requests_total",
		Help:      "Total HTTP requests handled by route template and status class.",
	}, []string{"method", "route", "status_class"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds by route template and status class.",
		Buckets:   core.DurationBuckets(),
	}, []string{"method", "route", "status_class"})
	inflight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "http_requests_in_flight",
		Help:      "Current HTTP requests in flight by route template.",
	}, []string{"method", "route"})
	if err := registry.Register(requests, duration, inflight); err != nil {
		return nil, err
	}
	return &Plugin{
		core:     core,
		requests: requests,
		duration: duration,
		inflight: inflight,
		handler:  promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError},
	}, nil
}

// Definition returns the built-in HTTP instrumentation's canonical immutable
// declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the built-in
// HTTP instrumentation and the neutral metrics plugin it registers into.
func Bundle() plugin.Bundle { return bundle }
