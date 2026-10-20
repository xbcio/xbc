package metrics

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	xbcconfig "github.com/xbcio/xbc/config"
)

// TestDefaultRuntimeCollectorsAreRegistered pins the default scrape contents:
// with no configuration both standard collectors are on, so a gather yields
// the canonical go_* and process_* families alongside build_info.
func TestDefaultRuntimeCollectorsAreRegistered(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	families := mustGather(t, p.Registry())
	if !hasFamily(families, "go_goroutines") {
		t.Fatalf("go_goroutines missing from %v", familyNames(families))
	}
	if !hasFamilyPrefix(families, "process_") {
		t.Fatalf("no process_* family in %v", familyNames(families))
	}
	if !hasFamily(families, "xbc_build_info") {
		t.Fatalf("build_info missing from %v", familyNames(families))
	}
}

// TestRuntimeCollectorsCanBeDisabled pins that turning both switches off
// removes exactly those families and leaves the framework's own series alone.
func TestRuntimeCollectorsCanBeDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runtime.GoCollector = false
	cfg.Runtime.ProcessCollector = false
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	families := mustGather(t, p.Registry())
	if hasFamily(families, "go_goroutines") {
		t.Fatalf("go_goroutines present with go_collector off: %v", familyNames(families))
	}
	if hasFamilyPrefix(families, "process_") {
		t.Fatalf("process_* present with process_collector off: %v", familyNames(families))
	}
	if !hasFamily(families, "xbc_build_info") {
		t.Fatalf("build_info missing from %v", familyNames(families))
	}
}

// TestRuntimeCollectorSwitchesAreIndependent pins that each switch controls
// only its own collector: one off leaves the other family intact.
func TestRuntimeCollectorSwitchesAreIndependent(t *testing.T) {
	cases := []struct {
		name             string
		goCollector      bool
		processCollector bool
	}{
		{name: "go collector off", goCollector: false, processCollector: true},
		{name: "process collector off", goCollector: true, processCollector: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Runtime.GoCollector = testCase.goCollector
			cfg.Runtime.ProcessCollector = testCase.processCollector
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			families := mustGather(t, p.Registry())
			if got := hasFamily(families, "go_goroutines"); got != testCase.goCollector {
				t.Fatalf("go_goroutines present = %t with go_collector = %t: %v", got, testCase.goCollector, familyNames(families))
			}
			if got := hasFamilyPrefix(families, "process_"); got != testCase.processCollector {
				t.Fatalf("process_* present = %t with process_collector = %t: %v", got, testCase.processCollector, familyNames(families))
			}
		})
	}
}

// TestTwoPluginsGatherRuntimeCollectorsIndependently pins that each Plugin
// owns an isolated registry: building two from the same config registers the
// standard collectors twice without a duplicate error.
func TestTwoPluginsGatherRuntimeCollectorsIndependently(t *testing.T) {
	cfg := DefaultConfig()
	first, err := New(cfg)
	if err != nil {
		t.Fatalf("first New() = %v", err)
	}
	second, err := New(cfg)
	if err != nil {
		t.Fatalf("second New() = %v", err)
	}
	for name, p := range map[string]*Plugin{"first": first, "second": second} {
		families := mustGather(t, p.Registry())
		if !hasFamily(families, "go_goroutines") {
			t.Fatalf("%s plugin: go_goroutines missing from %v", name, familyNames(families))
		}
		if !hasFamilyPrefix(families, "process_") {
			t.Fatalf("%s plugin: no process_* family in %v", name, familyNames(families))
		}
		if !hasFamily(families, "xbc_build_info") {
			t.Fatalf("%s plugin: build_info missing from %v", name, familyNames(families))
		}
	}
}

// TestRuntimeSwitchesBindTheirDocumentedKeysAndDefaults pins the configuration
// surface itself, not just DefaultConfig(): binding into a zero Config proves
// the default:"true" struct tags turn both collectors on for an absent
// section, and that go_collector/process_collector are the accepted YAML key
// spellings. A renamed key is a strict-decoding error (or leaves the tag
// default in place), so either way the assertion below fails.
func TestRuntimeSwitchesBindTheirDocumentedKeysAndDefaults(t *testing.T) {
	absent := bindMetricsConfig(t, map[string]any{})
	if !absent.Runtime.GoCollector || !absent.Runtime.ProcessCollector {
		t.Fatalf("empty plugins.metrics bound runtime = %+v, want both collectors on", absent.Runtime)
	}
	// The binder applies the tags while DefaultConfig serves as the plugin's
	// declared defaults; keep the two from diverging silently. Only Runtime is
	// compared because DurationBuckets has no tag representation and comes
	// from DefaultConfig alone.
	if got, want := absent.Runtime, DefaultConfig().Runtime; got != want {
		t.Fatalf("binder defaults %+v diverge from DefaultConfig() %+v", got, want)
	}

	disabled := bindMetricsConfig(t, map[string]any{
		"runtime": map[string]any{
			"go_collector":      false,
			"process_collector": false,
		},
	})
	if disabled.Runtime.GoCollector || disabled.Runtime.ProcessCollector {
		t.Fatalf("explicitly disabled runtime = %+v, want both collectors off", disabled.Runtime)
	}
}

func bindMetricsConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{"metrics": values},
	}, "XBC_METRICS_TEST_")
	if err != nil {
		t.Fatalf("NewEnvironment() = %v", err)
	}
	var cfg Config // zero value: only the struct-tag defaults may fill it
	if err := environment.Bind("plugins.metrics", &cfg); err != nil {
		t.Fatalf("Bind(plugins.metrics) = %v", err)
	}
	return cfg
}

func hasFamilyPrefix(families []*dto.MetricFamily, prefix string) bool {
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), prefix) {
			return true
		}
	}
	return false
}
