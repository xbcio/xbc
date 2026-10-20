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

// admissionKey is the identity of the admission gate collector's Definition.
// It is separate from Key because the Web plugin collects every web.Middleware
// exporter: a Definition that exports the middleware and also requires the Web
// plugin would close a dependency cycle. The collector exports nothing back to
// Web, so its own Definition can depend on it.
const admissionKey plugin.Key = "http-metrics-admission"

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

// admissionPlugin is the admission collector's primary value. It owns no
// lifecycle and exports nothing: its only effect is the collector registered in
// the neutral plugin's registry during construction.
type admissionPlugin struct{}

var (
	_ web.Middleware       = (*Plugin)(nil)
	_ web.RouteContributor = (*Plugin)(nil)
)

var (
	metricsInput = plugin.RefTo[*metricscore.Plugin](metricscore.Key)
	// admissionInput binds the Web plugin's read-only admission contract. The
	// instrumentation already observes the server it must scrape here, and a
	// composition that selects it without Web is invalid: the admission gate
	// refuses traffic before any contributed middleware runs, so these are the
	// only readings that remain visible during saturation.
	admissionInput = plugin.RefTo[web.AdmissionStatsSource](web.Key)
)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return newRequestInstrumentation(metricsInput.Get(ctx).Value)
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

// admissionDefinition registers the admission gate collector beside the request
// instruments. It is a separate Definition so it can require the Web plugin
// without closing the cycle Web's middleware collection would otherwise form.
// Selecting the bundle without Web fails at planning time with the framework's
// missing-producer diagnostic rather than silently omitting the series.
var admissionDefinition = plugin.Define(
	admissionKey,
	func(ctx plugin.BuildContext) (*admissionPlugin, error) {
		return newAdmissionPlugin(metricsInput.Get(ctx).Value, admissionInput.Get(ctx).Value)
	},
	plugin.Options[*admissionPlugin]{
		Activation: plugin.WhenConfigured("plugins." + metricscore.Key.String()),
		Inputs:     plugin.Inputs(metricsInput, admissionInput),
	},
)

// bundle selects the neutral metrics plugin alongside the built-in HTTP
// instrumentation. The instruments register into that plugin's registry, and
// RefTo would reject the instrumentation without it at planning time anyway.
var bundle = plugin.CombineBundles(
	metricscore.Bundle(),
	plugin.BundleOf(definition),
	plugin.BundleOf(admissionDefinition),
)

// New builds the built-in HTTP instrumentation over the neutral metrics plugin
// and registers its three bounded-cardinality request instruments plus the
// admission gate collector with that plugin's registry, so one endpoint serves
// them beside application collectors. The admission source is required: a
// request refused by the process-level gate runs no contributed middleware, so
// without this collector a saturated process would expose none of its refusals.
//
// The bundled Definitions split the same work in two, because Web collects the
// middleware exported by definition and a single Definition cannot both export
// it and depend on Web. New is the direct-embedding constructor and supplies
// both halves at once.
func New(core *metricscore.Plugin, admission web.AdmissionStatsSource) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("http-metrics: requires the metrics plugin")
	}
	if admission == nil {
		return nil, errors.New("http-metrics: requires an admission stats source")
	}
	value, err := newRequestInstrumentation(core)
	if err != nil {
		return nil, err
	}
	if err := registerAdmissionCollector(core, admission); err != nil {
		return nil, err
	}
	return value, nil
}

// newRequestInstrumentation builds the three bounded-cardinality request
// instruments and registers them with core's registry.
func newRequestInstrumentation(core *metricscore.Plugin) (*Plugin, error) {
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

// newAdmissionPlugin validates the admission collector's inputs and registers
// the collector with core's registry.
func newAdmissionPlugin(core *metricscore.Plugin, admission web.AdmissionStatsSource) (*admissionPlugin, error) {
	if admission == nil {
		return nil, errors.New("http-metrics-admission: requires an admission stats source")
	}
	if err := registerAdmissionCollector(core, admission); err != nil {
		return nil, err
	}
	return &admissionPlugin{}, nil
}

// registerAdmissionCollector registers the admission gate collector with core's
// registry. A nil source is an error rather than a silently omitted production
// signal: a process-level gate's refusals are visible nowhere else.
func registerAdmissionCollector(core *metricscore.Plugin, admission web.AdmissionStatsSource) error {
	if admission == nil {
		return errors.New("http-metrics: requires an admission stats source")
	}
	registry := core.Registry()
	if registry == nil {
		return errors.New("http-metrics: the metrics plugin has no registry")
	}
	return registry.Register(newAdmissionCollector(core, admission))
}

// Definition returns the built-in HTTP instrumentation's canonical immutable
// declaration handle. It is the middleware half of Bundle: it exports the
// request middleware and scrape route and does not depend on Web.
func Definition() plugin.Definition { return definition }

// AdmissionDefinition returns the admission gate collector's canonical
// declaration handle. It is the collector half of Bundle: it exports nothing
// back to Web and depends on the Web plugin's AdmissionStatsSource. A private
// catalog that selects Definition directly must select this Definition
// alongside it, or the admission series will be absent.
func AdmissionDefinition() plugin.Definition { return admissionDefinition }

// Bundle returns the side-effect-free explicit composition of the built-in
// HTTP instrumentation, its admission gate collector, and the neutral metrics
// plugin they register into.
func Bundle() plugin.Bundle { return bundle }
