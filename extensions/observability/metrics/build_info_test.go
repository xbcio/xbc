package metrics

import (
	"runtime/debug"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// TestBuildInfoPublishesOneIdentitySeries pins the P1 metric contract: the
// plugin registers exactly one build_info series, valued 1, labelled with the
// process identity -- so a dashboard joins it onto any other series to answer
// "which build do these numbers come from". Deliberately absent is an
// instance label: the instance belongs to the scrape target, and baking it in
// would multiply every other series by the replica count.
func TestBuildInfoPublishesOneIdentitySeries(t *testing.T) {
	p, err := newPlugin(plugin.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	scrape := func() *dto.MetricFamily {
		t.Helper()
		family := familyByName(t, mustGather(t, p.Registry()), "xbc_build_info")
		if got := family.GetType(); got != dto.MetricType_GAUGE {
			t.Fatalf("build_info type = %v, want gauge", got)
		}
		return family
	}

	metrics := scrape().GetMetric()
	if len(metrics) != 1 {
		t.Fatalf("build_info series = %d, want exactly 1", len(metrics))
	}
	if got := metrics[0].GetGauge().GetValue(); got != 1 {
		t.Fatalf("build_info value = %v, want 1", got)
	}
	labels := labelValues(metrics[0])
	if got := labels["service"]; got != "orders" {
		t.Fatalf("build_info service label = %q, want %q", got, "orders")
	}
	if got := labels["version"]; got != "v1.2.3" {
		t.Fatalf("build_info version label = %q, want %q", got, "v1.2.3")
	}
	if labels["go_version"] == "" {
		t.Fatal("build_info must carry the toolchain version it was built with")
	}
	if _, ok := labels["instance"]; ok {
		t.Fatal("build_info must not carry an instance label: the scrape target supplies it")
	}

	// A second scrape must be the same one series, not one more per scrape.
	if again := scrape().GetMetric(); len(again) != 1 {
		t.Fatalf("second scrape produced %d build_info series, want 1", len(again))
	}
}

// TestDefinitionFactoryCarriesTheFrameworkIdentity pins the one seam this
// file's direct newPlugin tests cannot see: the Definition factory itself must
// hand newPlugin the identity a BuildContext carries. Those tests inject the
// identity straight into newPlugin, so a factory passing a zero identity
// instead would leave every one of them green while every assembled
// application silently lost the service and version labels. This test builds
// through the real planning and construction path -- BuildPlan and Construct,
// the same machinery a boot runs -- and reads the labels off the constructed
// Plugin's own registry.
func TestDefinitionFactoryCarriesTheFrameworkIdentity(t *testing.T) {
	environment, err := config.NewEnvironment(
		map[string]any{"plugins": map[string]any{Key.String(): map[string]any{}}},
		"XBC_METRICS_DEFINITION_TEST_UNSET_")
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		Service: plugin.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"},
	})
	if err != nil {
		t.Fatalf("Construct: %v", err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key})
	if !ok {
		t.Fatal("the metrics instance was not constructed")
	}
	p, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}

	family := familyByName(t, mustGather(t, p.Registry()), "xbc_build_info")
	series := family.GetMetric()
	if len(series) != 1 {
		t.Fatalf("build_info series = %d, want exactly 1", len(series))
	}
	labels := labelValues(series[0])
	if got := labels["service"]; got != "orders" {
		t.Fatalf("build_info service label = %q, want %q", got, "orders")
	}
	if got := labels["version"]; got != "v1.2.3" {
		t.Fatalf("build_info version label = %q, want %q", got, "v1.2.3")
	}
}

// TestBuildInfoCarriesTheFullRecordedRevision pins the revision label's
// documented shape: the VCS revision the build recorded, in full -- it is a
// label selected on, not a version shown to a human -- and the empty string
// for a build that recorded none.
func TestBuildInfoCarriesTheFullRecordedRevision(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	previous := readBuildInfo
	readBuildInfo = func() *debug.BuildInfo {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}}}
	}
	t.Cleanup(func() { readBuildInfo = previous })

	p, err := newPlugin(plugin.ServiceIdentity{Name: "orders"}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	metrics := familyByName(t, mustGather(t, p.Registry()), "xbc_build_info").GetMetric()
	if len(metrics) != 1 {
		t.Fatalf("build_info series = %d, want exactly 1", len(metrics))
	}
	if got := labelValues(metrics[0])["revision"]; got != revision {
		t.Fatalf("build_info revision = %q, want the full recorded %q", got, revision)
	}

	readBuildInfo = func() *debug.BuildInfo { return nil }
	p, err = newPlugin(plugin.ServiceIdentity{Name: "orders"}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	metrics = familyByName(t, mustGather(t, p.Registry()), "xbc_build_info").GetMetric()
	if got := labelValues(metrics[0])["revision"]; got != "" {
		t.Fatalf("build_info revision = %q for a build that recorded none, want empty", got)
	}
}

// TestBuildInfoJoinsTheConfiguredNamespace pins that the series follows the
// plugin's namespace like every other instrument: "orders" yields
// "orders_build_info".
func TestBuildInfoJoinsTheConfiguredNamespace(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Namespace = "orders"
	p, err := newPlugin(plugin.ServiceIdentity{Name: "orders"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	families := mustGather(t, p.Registry())
	if !hasFamily(families, "orders_build_info") {
		t.Fatalf("namespaced build_info series missing from %v", familyNames(families))
	}
	if hasFamily(families, "build_info") {
		t.Fatal("build_info was registered without its namespace")
	}
}

func familyByName(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	t.Fatalf("metric family %q missing from %v", name, familyNames(families))
	return nil
}

func familyNames(families []*dto.MetricFamily) []string {
	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
	}
	return names
}

func labelValues(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		labels[pair.GetName()] = pair.GetValue()
	}
	return labels
}
