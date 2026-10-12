package starter_test

import (
	"reflect"
	"regexp"
	"slices"
	"sort"
	"testing"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/grpc/starter"
)

// dormantMemberPaths returns the activation path of every Definition in the
// starter's Bundles that stays dormant until a configuration section exists,
// derived from the Definitions themselves. Deriving it rather than listing it
// is the point: a prelude member added later with the same activation policy
// is picked up here without anyone remembering to edit this file.
func dormantMemberPaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	for _, bundle := range starter.GRPC().Bundles() {
		for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(bundle)) {
			descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
			if !ok {
				t.Fatal("a Bundle returned by the starter must hold canonical Definitions")
			}
			if descriptor.Activation.Kind != pluginmodel.ActivationConfigured {
				continue
			}
			if descriptor.Activation.Path == "" {
				t.Fatal("a configured activation without a path could not be satisfied by any configuration")
			}
			paths = append(paths, descriptor.Activation.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

var togglePath = regexp.MustCompile(`^plugins\.[a-z0-9-]+\.enabled$`)

// enabledPaths returns the section each entry of the defaults layer switches
// on, rejecting a spelling the configuration layer would not read back.
func enabledPaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	for path, value := range starter.GRPC().Defaults().Values {
		if value != true {
			t.Fatalf("%s = %#v: the baseline switches a capability on; it does not restate the capability's own values", path, value)
		}
		if !togglePath.MatchString(path) {
			t.Fatalf("%s: an entry that is not a plugin toggle cannot activate anything", path)
		}
		paths = append(paths, path[:len(path)-len(".enabled")])
	}
	sort.Strings(paths)
	return paths
}

// TestGRPCBaselineTurnsOnExactlyTheDormantMembers is the whole promise of the
// package in one assertion. A starter that misses a member promises a baseline
// it does not deliver; one that enables something else claims a capability the
// application never selected. Both directions are failures, so both are
// compared against the Definitions the starter actually contributes.
func TestGRPCBaselineTurnsOnExactlyTheDormantMembers(t *testing.T) {
	dormant := dormantMemberPaths(t)
	if len(dormant) == 0 {
		t.Fatal("the prelude is expected to have members that stay dormant; if that ever stops being true this test proves nothing")
	}
	if enabled := enabledPaths(t); !reflect.DeepEqual(dormant, enabled) {
		t.Fatalf("enabled %v, dormant %v: the baseline must switch on every dormant member of the Bundles it selects, and nothing else", enabled, dormant)
	}
}

// TestGRPCBaselineSelectsNoTelemetryModule pins the dependency boundary the
// package documents: metrics and tracing are independently versioned modules,
// and a baseline inside transport/grpc that selected them would pull
// Prometheus and OpenTelemetry into the transport's own closure.
func TestGRPCBaselineSelectsNoTelemetryModule(t *testing.T) {
	bundles := starter.GRPC().Bundles()
	if len(bundles) != 1 {
		t.Fatalf("Bundles() = %d, want exactly the prelude", len(bundles))
	}
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundles[0]))
	if len(entries) == 0 {
		t.Fatal("the baseline selects the prelude's Definitions")
	}
	for _, entry := range entries {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		if !ok {
			t.Fatal("a Bundle returned by the starter must hold canonical Definitions")
		}
		if key := descriptor.Key.String(); slices.Contains([]string{"metrics", "grpc-metrics", "tracing", "tracing-grpc"}, key) {
			t.Fatalf("the baseline selects %s; telemetry is selected by the composition root beside it", key)
		}
	}
}

// TestGRPCBaselineDefaultsAreAFreshLayerPerCall pins the ownership of the
// returned map. A shared one would be mutable state behind an interface that
// claims to be composition data.
func TestGRPCBaselineDefaultsAreAFreshLayerPerCall(t *testing.T) {
	first := starter.GRPC().Defaults()
	second := starter.GRPC().Defaults()

	if len(first.Values) == 0 {
		t.Fatal("the defaults layer is empty")
	}
	for path := range first.Values {
		delete(first.Values, path)
	}
	if len(second.Values) == 0 {
		t.Fatal("a caller mutating one layer must not empty the next one")
	}
	if second.Label != "grpc starter" {
		t.Fatalf("Label = %q: the layer names its contributor, which is what doctor prints as the origin of a section", second.Label)
	}
}
