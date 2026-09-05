// Package metrics provides application-private Prometheus instrumentation.
//
// # Usage
//
// A business plugin declares a dependency on Registry and registers collectors
// during Init. Those collectors are then served by the same endpoint as the
// built-in bounded-cardinality HTTP metrics:
//
//	type Orders struct {
//		completed prometheus.Counter
//	}
//
//	func (*Orders) Dependencies() plugin.Deps {
//		return plugin.Deps{Types: []plugin.Dep{plugin.Need[*metrics.Registry]()}}
//	}
//
//	func (p *Orders) Init(ctx *plugin.Context) error {
//		completed := prometheus.NewCounter(prometheus.CounterOpts{
//			Name: "orders_completed_total",
//			Help: "Total number of completed orders.",
//		})
//		if err := plugin.MustGet[*metrics.Registry](ctx).Register(completed); err != nil {
//			return err
//		}
//		p.completed = completed
//		return nil
//	}
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
// The HTTP endpoint is disabled by default. When enabled, it authorizes only a
// direct loopback peer or a constant-time token match and never trusts
// forwarded client-address headers. The registry remains owned by the metrics
// plugin for its full lifecycle, including Web connection draining. Importing
// this implementation package has no catalog side effects; import
// github.com/xbcio/xbc/transport/web/integrations/metrics/autoload explicitly for
// default-catalog registration.
package metrics
