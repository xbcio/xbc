package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestPrometheusRegistererAdaptsThePrivateRegistry(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	registerer := PrometheusRegisterer(p)
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_prometheus_adapter_total",
		Help: "Counter exercising the prometheus.Registerer adapter.",
	})

	if err := registerer.Register(counter); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	counter.Inc()

	families, err := p.Gather()
	if err != nil {
		t.Fatalf("Gather() = %v", err)
	}
	if !hasFamily(families, "xbc_metrics_prometheus_adapter_total") {
		t.Fatal("the adapter's registration did not reach the private registry")
	}

	if !registerer.Unregister(counter) {
		t.Fatal("Unregister() reported an existing collector absent")
	}
	families, err = p.Gather()
	if err != nil {
		t.Fatalf("Gather() after Unregister = %v", err)
	}
	if hasFamily(families, "xbc_metrics_prometheus_adapter_total") {
		t.Fatal("Unregister() did not remove the collector from the private registry")
	}
}

func TestPrometheusRegistererMustRegisterPanicsOnDuplicate(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	registerer := PrometheusRegisterer(p)
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_must_register_total",
		Help: "Counter exercising MustRegister's duplicate handling.",
	})
	if err := registerer.Register(counter); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister() did not panic on a duplicate collector")
		}
	}()
	registerer.MustRegister(counter)
}

func TestPrometheusRegistererToleratesNilContract(t *testing.T) {
	registerer := PrometheusRegisterer(nil)
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_nil_adapter_total",
		Help: "Counter exercising the nil adapter.",
	})

	if err := registerer.Register(counter); err == nil {
		t.Fatal("Register() on a nil adapter returned no error")
	}
	if registerer.Unregister(counter) {
		t.Fatal("Unregister() on a nil adapter reported a collector present")
	}
}
