// Package metrics provides protocol-neutral, application-private Prometheus
// instrumentation for XBC.
//
// This package owns the registry: configuration bound from plugins.metrics,
// one private collector registry, and the Registerer contract that exposes it.
// It binds to no transport, so a Web middleware and a gRPC interceptor can
// each install their own instruments into it.
//
// # Usage
//
// Any plugin publishes its own collectors through a construction-time input on
// Key:
//
//	var registerer = plugin.RefTo[metrics.Registerer](metrics.Key)
//
//	type Orders struct {
//		completed prometheus.Counter
//	}
//
//	var ordersDefinition = plugin.Define(
//		"orders",
//		func(ctx plugin.BuildContext) (*Orders, error) {
//			completed := prometheus.NewCounter(prometheus.CounterOpts{
//				Name: "orders_completed_total",
//				Help: "Total number of completed orders.",
//			})
//			if err := registerer.Get(ctx).Value.Register(completed); err != nil {
//				return nil, err
//			}
//			return &Orders{completed: completed}, nil
//		},
//		plugin.Options[*Orders]{Inputs: plugin.Inputs(registerer)},
//	)
//
//	func (p *Orders) MarkCompleted() {
//		p.completed.Inc()
//	}
//
// Every Plugin owns an isolated registry. It never registers collectors with
// prometheus.DefaultRegisterer and never exposes the process-wide default
// gatherer. Collector registration is atomic per Register call and duplicate
// descriptor errors are returned instead of panicking.
//
// # Runtime collectors
//
// The plugin also registers the standard Go runtime and process collectors
// into its private registry, behind two switches that default to on:
// plugins.metrics.runtime.go_collector yields the go_* series and
// plugins.metrics.runtime.process_collector the process_* series. Both keep
// their canonical names -- no namespace or subsystem prefix is applied --
// because dashboards and recording rules expect go_goroutines,
// go_memstats_* and process_resident_memory_bytes as they are. These are
// process-level series without service labels: they describe the process, not
// the service identity, which stays on build_info and the business
// instruments. A deployment that must minimize its metric surface turns either
// switch off.
//
// The HTTP server instrumentation and the exposition endpoint live in the Web
// adapter at
// github.com/xbcio/xbc/transport/web/extensions/observability/metrics, which
// applies the Namespace, Subsystem, and DurationBuckets settings to the
// instruments it owns and serves the registry through the endpoint that
// plugins.metrics.http enables. The endpoint is disabled by default; when
// enabled, access control is determined by the application's authentication
// policy. The registry remains owned by this plugin for its full lifecycle,
// including Web connection draining. Importing this implementation package has
// no catalog side effects; import
// github.com/xbcio/xbc/extensions/observability/metrics/autoload explicitly
// for default-catalog registration.
package metrics
