package metrics_test

import (
	"reflect"
	"testing"

	neutralmetrics "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if neutralmetrics.Definition() == zeroDefinition || neutralmetrics.Definition() != neutralmetrics.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := neutralmetrics.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, neutralmetrics.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary metrics import or Bundle() mutated the autoload composition")
	}
}
