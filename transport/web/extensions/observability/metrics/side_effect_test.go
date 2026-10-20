package metrics_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	metricsplugin "github.com/xbcio/xbc/transport/web/extensions/observability/metrics"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if metricsplugin.Definition() == zeroDefinition || metricsplugin.Definition() != metricsplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := metricsplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, metricsplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}

// TestAdmissionDefinitionIsCanonicalAndDistinct pins the collector-half
// accessor: it must be a real, stable Definition and it must not be the
// middleware Definition, or a private catalog following the package
// documentation would silently select the instrumented middleware for both
// halves and never register the admission collector.
func TestAdmissionDefinitionIsCanonicalAndDistinct(t *testing.T) {
	var zeroDefinition plugin.Definition
	admission := metricsplugin.AdmissionDefinition()
	if admission == zeroDefinition || admission != metricsplugin.AdmissionDefinition() {
		t.Fatal("AdmissionDefinition() must return one non-zero canonical handle")
	}
	if admission == metricsplugin.Definition() {
		t.Fatal("AdmissionDefinition() must identify the collector half, not the request instrumentation")
	}
}
