// Package metrics provides application-private Prometheus instrumentation.
//
// # Usage
//
// A business plugin declares an input on this Definition's primary value and
// registers its collectors with the private registry that value exposes. Those
// collectors are then served by the same endpoint as the built-in
// bounded-cardinality HTTP metrics:
//
//	var metricsInput = plugin.RefTo[*metrics.Plugin](metrics.Key)
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
//			if err := metricsInput.Get(ctx).Value.Registry().Register(completed); err != nil {
//				return nil, err
//			}
//			return &Orders{completed: completed}, nil
//		},
//		plugin.Options[*Orders]{Inputs: plugin.Inputs(metricsInput)},
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
// The HTTP endpoint is disabled by default. When enabled, access control is
// determined by the application's authentication policy. The registry remains
// owned by the metrics plugin for its full lifecycle, including Web connection
// draining. Importing this implementation package has no catalog side effects;
// import github.com/xbcio/xbc/transport/web/extensions/observability/metrics/autoload
// explicitly for default-catalog registration.
package metrics
