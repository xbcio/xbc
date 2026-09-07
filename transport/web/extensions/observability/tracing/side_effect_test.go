package tracing_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if tracing.Definition() == zeroDefinition || tracing.Definition() != tracing.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := tracing.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, tracing.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary tracing import or Bundle() mutated the autoload composition")
	}
}
