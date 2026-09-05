package metrics_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	metricsplugin "github.com/xbcio/xbc/transport/web/integrations/metrics"
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
