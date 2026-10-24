package metrics

import (
	"errors"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zeroDefinition plugin.Definition
	if Definition() == zeroDefinition || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	// BundleOf records the caller origin, so rebuilding the same Bundle from a
	// test file can never compare equal; only non-emptiness and stability hold.
}

func TestNewBuildsPrivateRegistry(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if p.Registry() == nil {
		t.Fatal("New() left the registry nil")
	}
}

func TestPrivateRegistryNeverPollutesDefaultGatherer(t *testing.T) {
	before, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	probe := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_private_probe_total",
		Help: "Probe counter that must stay out of the process default registry.",
	})
	if err := p.Register(probe); err != nil {
		t.Fatal(err)
	}
	probe.Inc()
	after, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("default gatherer families changed from %d to %d", len(before), len(after))
	}
	for _, family := range after {
		if family.GetName() == "xbc_metrics_private_probe_total" {
			t.Fatal("collector registered with the plugin leaked into the default gatherer")
		}
	}
	families, err := p.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	if !hasFamily(families, "xbc_metrics_private_probe_total") {
		t.Fatal("plugin registry did not gather the registered collector")
	}
}

func TestRegistryRejectsDuplicatesWithoutPanic(t *testing.T) {
	registry := newRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_duplicate_total",
		Help: "Counter registered twice to exercise duplicate handling.",
	})
	if err := registry.Register(counter); err != nil {
		t.Fatalf("first Register() = %v", err)
	}
	err := registry.Register(counter)
	var duplicate prometheus.AlreadyRegisteredError
	if !errors.As(err, &duplicate) {
		t.Fatalf("second Register() = %v, want AlreadyRegisteredError", err)
	}
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "xbc_metrics_after_failure",
		Help: "Gauge registered after a failed batch to prove the batch rolled back.",
	})
	if err := registry.Register(gauge); err != nil {
		t.Fatalf("Register() after rejected duplicate = %v", err)
	}
}

func TestRegisterRejectsNilCollectorWithoutPanic(t *testing.T) {
	registry := newRegistry()
	if err := registry.Register(nil); err == nil {
		t.Fatal("Register(nil) returned nil error")
	}
}

func TestGatherRejectsAnUninitializedRegistry(t *testing.T) {
	var nilRegistry *Registry
	if _, err := nilRegistry.Gather(); err == nil {
		t.Fatal("Gather() on a nil registry returned nil error")
	}

	var zero Registry
	if _, err := zero.Gather(); err == nil {
		t.Fatal("Gather() on a zero-value registry returned nil error")
	}
}

func TestPluginRegistererDelegatesToRegistry(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xbc_metrics_contract_total",
		Help: "Counter exercising the Registerer contract through the plugin.",
	})
	var registerer Registerer = p
	if err := registerer.Register(counter); err != nil {
		t.Fatalf("Registerer.Register() = %v", err)
	}
	if !hasFamily(mustGather(t, p.Registry()), "xbc_metrics_contract_total") {
		t.Fatal("Registerer.Register() did not reach the plugin registry")
	}
	if !registerer.Unregister(counter) {
		t.Fatal("Registerer.Unregister() reported an existing collector absent")
	}
	if registerer.Unregister(counter) {
		t.Fatal("Registerer.Unregister() reported a removed collector present")
	}
}

func TestAccessorsExposeNormalizedSettings(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Namespace(); got != "xbc" {
		t.Fatalf("Namespace() = %q, want %q", got, "xbc")
	}
	if got := p.Subsystem(); got != "" {
		t.Fatalf("Subsystem() = %q, want empty", got)
	}
	buckets := p.DurationBuckets()
	if !reflect.DeepEqual(buckets, prometheus.DefBuckets) {
		t.Fatalf("DurationBuckets() = %v, want DefBuckets", buckets)
	}
	buckets[0] = -1
	if again := p.DurationBuckets(); again[0] == -1 {
		t.Fatal("DurationBuckets() exposed the plugin's own slice")
	}
	httpConfig := p.HTTP()
	if httpConfig.Enabled || httpConfig.Path != "/metrics" {
		t.Fatalf("HTTP() = %+v, want disabled default path", httpConfig)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	invalid := DefaultConfig()
	invalid.Namespace = "Invalid-Namespace"
	if _, err := New(invalid); err == nil {
		t.Fatal("New() accepted an invalid namespace")
	}
}

func mustGather(t *testing.T, registry *Registry) []*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return families
}

func hasFamily(families []*dto.MetricFamily, name string) bool {
	for _, family := range families {
		if family.GetName() == name {
			return true
		}
	}
	return false
}
