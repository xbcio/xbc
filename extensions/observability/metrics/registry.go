package metrics

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Registry is an application-private collector registry. Register is
// all-or-nothing for each call and returns duplicate/descriptor errors instead
// of panicking. Collectors registered here are served by the Web adapter's
// HTTP endpoint when it is enabled and never enter Prometheus's process-wide
// default registry.
type Registry struct {
	mu    sync.Mutex
	inner *prometheus.Registry
}

func newRegistry() *Registry {
	return &Registry{inner: prometheus.NewRegistry()}
}

// Register adds collectors atomically. If one collector fails, collectors
// added earlier by the same call are removed before the error is returned.
func (r *Registry) Register(collectors ...prometheus.Collector) error {
	if r == nil || r.inner == nil {
		return fmt.Errorf("metrics: registry is not initialized")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	registered := make([]prometheus.Collector, 0, len(collectors))
	for i, collector := range collectors {
		if collector == nil || reflect.ValueOf(collector).Kind() == reflect.Ptr && reflect.ValueOf(collector).IsNil() {
			for _, previous := range registered {
				r.inner.Unregister(previous)
			}
			return fmt.Errorf("metrics: collector %d is nil", i)
		}
		if err := r.inner.Register(collector); err != nil {
			for _, previous := range registered {
				r.inner.Unregister(previous)
			}
			return fmt.Errorf("metrics: register collector %d: %w", i, err)
		}
		registered = append(registered, collector)
	}
	return nil
}

// Unregister removes a collector from this private registry.
func (r *Registry) Unregister(collector prometheus.Collector) bool {
	if r == nil || r.inner == nil || collector == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner.Unregister(collector)
}

// Gather collects the metrics of every registered collector, implementing
// prometheus.Gatherer over this private registry for the transport adapters
// that serve them.
func (r *Registry) Gather() ([]*dto.MetricFamily, error) {
	if r == nil || r.inner == nil {
		return nil, fmt.Errorf("metrics: registry is not initialized")
	}
	return r.inner.Gather()
}

// Registerer is the neutral registration contract this module exports over its
// private registry: a plugin that publishes its own collectors reaches the
// registry through a construction-time input on Key instead of a transport
// dependency, and the application's metrics endpoint gathers whatever it
// registered.
//
// Register keeps the concrete Registry's all-or-nothing call semantics rather
// than prometheus.Registerer's single-collector shape: a caller registering
// several collectors that describe one component gets either all of them or
// none, and duplicate or invalid descriptors come back as errors instead of
// panics.
type Registerer interface {
	// Register adds the collectors atomically.
	Register(collectors ...prometheus.Collector) error
	// Unregister removes a previously registered collector, reporting whether
	// it was present.
	Unregister(collector prometheus.Collector) bool
}

// Gatherer is the neutral gathering contract this module exports over its
// private registry. It is the read half of Registerer: a consumer that serves
// metrics through its own HTTP stack -- an embedded server, a framework with
// its own scrape endpoint -- takes Gatherer as an input and renders the
// returned families itself, so the registry stays private and no process-wide
// Prometheus state is touched.
//
// The signature is identical to prometheus.Gatherer, so a value satisfying
// this contract can also be passed to promhttp.HandlerFor and
// prometheus.Gatherers directly.
type Gatherer interface {
	// Gather returns the metric families of every registered collector.
	Gather() ([]*dto.MetricFamily, error)
}

var _ Gatherer = (*Registry)(nil)
var _ prometheus.Gatherer = (*Registry)(nil)
