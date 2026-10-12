package placement

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/xbcio/xbc/config"
	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestStatsCollectorPublishesTheDocumentedSeries(t *testing.T) {
	snapshot := Stats{
		Source:        placementSourceLease,
		Instance:      "orders-1",
		RenewFailures: 3,
		Held: []Held{
			{Workload: "report", Slot: 1, Age: 30 * time.Second},
			{Workload: "sast", Slot: 0, Age: 90 * time.Second},
		},
		Declared: []Declared{
			{Workload: "report", Replicas: 3},
			{Workload: "sast", Replicas: 1, Exclusive: true},
		},
	}
	core, err := metricscore.New(metricscore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Register(newStatsCollector(func() Stats { return snapshot })); err != nil {
		t.Fatal(err)
	}
	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		name   string
		labels map[string]string
		value  float64
	}{
		{"xbc_workload_held", map[string]string{"workload": "report"}, 1},
		{"xbc_workload_held", map[string]string{"workload": "sast"}, 1},
		{"xbc_workload_lease_age_seconds", map[string]string{"workload": "report"}, 30},
		{"xbc_workload_lease_age_seconds", map[string]string{"workload": "sast"}, 90},
		{"xbc_workload_lease_renew_failures_total", nil, 3},
		{"xbc_workload_declared_replicas", map[string]string{"workload": "report", "exclusive": "false"}, 3},
		{"xbc_workload_declared_replicas", map[string]string{"workload": "sast", "exclusive": "true"}, 1},
		{"xbc_placement_standby", nil, 0},
	}
	for _, expected := range want {
		value, ok := sampleValue(families, expected.name, expected.labels)
		if !ok {
			t.Errorf("%s%v: series not found", expected.name, expected.labels)
			continue
		}
		if value != expected.value {
			t.Errorf("%s%v = %v, want %v", expected.name, expected.labels, value, expected.value)
		}
	}

	// The instance is the process identity, not a series label: Prometheus
	// joins these series to a process through its target labels.
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "instance" {
					t.Fatalf("%s carries an instance label", family.GetName())
				}
			}
		}
	}
}

func TestStatsCollectorReportsStandbyWithoutHeldSlots(t *testing.T) {
	core, err := metricscore.New(metricscore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Register(newStatsCollector(func() Stats { return Stats{Standby: true} })); err != nil {
		t.Fatal(err)
	}
	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	if value, ok := sampleValue(families, "xbc_placement_standby", nil); !ok || value != 1 {
		t.Fatalf("xbc_placement_standby = %v (found=%t), want 1", value, ok)
	}
	if value, ok := sampleValue(families, "xbc_workload_lease_renew_failures_total", nil); !ok || value != 0 {
		t.Fatalf("xbc_workload_lease_renew_failures_total = %v (found=%t), want 0", value, ok)
	}
	for _, family := range families {
		if family.GetName() == "xbc_workload_held" {
			t.Fatal("a standby published held series")
		}
	}
}

func TestDefinitionBridgesStatsIntoTheMetricsPlugin(t *testing.T) {
	mustNew(t, newMemoryLocker())
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"metrics": map[string]any{"namespace": "xbc"},
		},
	}, "XBC_PLACEMENT_METRICS_TEST_UNSET_")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{metricscore.Bundle(), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatal(err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: metricscore.Key, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the metrics plugin was not constructed")
	}
	core, ok := instance.Primary().(*metricscore.Plugin)
	if !ok {
		t.Fatalf("metrics primary is %T", instance.Primary())
	}
	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sampleValue(families, "xbc_placement_standby", nil); !ok {
		t.Error("the placement bridge was not registered with the metrics plugin")
	}
	if _, ok := sampleValue(families, "xbc_workload_lease_renew_failures_total", nil); !ok {
		t.Error("the placement bridge did not publish the renewal-failure counter")
	}
}

func TestDefinitionConstructsWithoutTheMetricsPlugin(t *testing.T) {
	mustNew(t, newMemoryLocker())
	environment, err := config.NewEnvironment(nil, "XBC_PLACEMENT_METRICS_TEST_UNSET_")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}); !ok {
		t.Fatal("the placement plugin was not constructed")
	}
}

// sampleValue finds the value of one series in a gathered family set.
func sampleValue(families []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, labels) {
				continue
			}
			if metric.GetCounter() != nil {
				return metric.GetCounter().GetValue(), true
			}
			return metric.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func labelsMatch(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, label := range metric.GetLabel() {
		if labels[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}
