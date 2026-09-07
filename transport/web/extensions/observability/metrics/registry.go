package metrics

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Registry is an application-private collector registry. Register is
// all-or-nothing for each call and returns duplicate/descriptor errors instead
// of panicking. Collectors registered here are exported by this plugin's HTTP
// endpoint and never enter Prometheus's process-wide default registry.
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
