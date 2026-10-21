package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusRegisterer adapts a Registerer to Prometheus's standard
// registration contract.
//
// The two shapes differ in one place: Registerer registers several collectors
// atomically so a component publishes either all of its series or none, while
// prometheus.Registerer registers one collector per call. A consumer whose
// library only accepts prometheus.Registerer -- promauto, a library's own
// instrumentation constructor, a framework wiring metrics through the standard
// interface -- can therefore still register into this module's private
// registry: each collector it passes arrives as its own all-or-nothing call.
//
// MustRegister keeps prometheus.Registerer's panicking semantics and is not a
// silent fallback; use Register when the error must be handled.
func PrometheusRegisterer(r Registerer) prometheus.Registerer {
	return prometheusRegisterer{inner: r}
}

type prometheusRegisterer struct {
	inner Registerer
}

func (r prometheusRegisterer) Register(collector prometheus.Collector) error {
	if r.inner == nil {
		return fmt.Errorf("metrics: registerer is not initialized")
	}
	return r.inner.Register(collector)
}

func (r prometheusRegisterer) MustRegister(collectors ...prometheus.Collector) {
	for _, collector := range collectors {
		if err := r.Register(collector); err != nil {
			panic(err)
		}
	}
}

func (r prometheusRegisterer) Unregister(collector prometheus.Collector) bool {
	if r.inner == nil {
		return false
	}
	return r.inner.Unregister(collector)
}

var _ prometheus.Registerer = prometheusRegisterer{}
