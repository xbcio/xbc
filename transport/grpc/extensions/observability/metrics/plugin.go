package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Key is the stable Definition and interceptor identity of the built-in gRPC
// instrumentation. It is distinct from the neutral metrics plugin's own key,
// metrics, which owns the plugins.metrics section: the instrumentation has no
// configuration of its own and reads its namespace, subsystem, and duration
// buckets from that plugin.
//
// The name is deliberately read "grpc" first, the same transport-first
// spelling as the Web half's http-metrics: the environment tokens of a
// "-grpc" suffix would name the neutral section's own sub-path rather than
// this plugin.
const Key plugin.Key = "grpc-metrics"

// Plugin instruments gRPC traffic on the neutral metrics plugin's private
// registry. The same primary value contributes both the unary and the stream
// interceptor.
type Plugin struct {
	core     *metricscore.Plugin
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
}

var (
	_ grpctransport.UnaryInterceptor  = (*Plugin)(nil)
	_ grpctransport.StreamInterceptor = (*Plugin)(nil)
)

var metricsInput = plugin.RefTo[*metricscore.Plugin](metricscore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return New(metricsInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		// The gRPC instrumentation follows the metrics plugin it feeds:
		// configuring plugins.metrics turns both on, and leaving it out leaves
		// both dormant.
		Activation: plugin.WhenConfigured("plugins." + metricscore.Key.String()),
		Inputs:     plugin.Inputs(metricsInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.UnaryInterceptor](func(value *Plugin) grpctransport.UnaryInterceptor { return value }),
			plugin.ExportAs[grpctransport.StreamInterceptor](func(value *Plugin) grpctransport.StreamInterceptor { return value }),
		),
	},
)

// bundle selects the neutral metrics plugin alongside the built-in gRPC
// instrumentation. The instruments register into that plugin's registry, and
// RefTo would reject the instrumentation without it at planning time anyway.
var bundle = plugin.CombineBundles(metricscore.Bundle(), plugin.BundleOf(definition))

// New builds the built-in gRPC instrumentation over the neutral metrics plugin
// and registers its three bounded-cardinality instruments with that plugin's
// registry, so one exposition serves them beside application collectors.
func New(core *metricscore.Plugin) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("grpc-metrics: requires the metrics plugin")
	}
	registry := core.Registry()
	if registry == nil {
		return nil, errors.New("grpc-metrics: the metrics plugin has no registry")
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "grpc_requests_total",
		Help:      "Total gRPC calls handled by service, method, and status code.",
	}, []string{"service", "method", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "grpc_request_duration_seconds",
		Help:      "gRPC call duration in seconds by service, method, and status code.",
		Buckets:   core.DurationBuckets(),
	}, []string{"service", "method", "code"})
	inflight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: core.Namespace(),
		Subsystem: core.Subsystem(),
		Name:      "grpc_requests_in_flight",
		Help:      "Current gRPC calls in flight by service and method.",
	}, []string{"service", "method"})
	if err := registry.Register(requests, duration, inflight); err != nil {
		return nil, err
	}
	return &Plugin{
		core:     core,
		requests: requests,
		duration: duration,
		inflight: inflight,
	}, nil
}

// Definition returns the built-in gRPC instrumentation's canonical immutable
// declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the built-in
// gRPC instrumentation and the neutral metrics plugin it registers into.
func Bundle() plugin.Bundle { return bundle }
