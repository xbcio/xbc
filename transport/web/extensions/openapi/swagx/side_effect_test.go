package swagx_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/openapi/swagx"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if swagx.Definition() == zeroDefinition || swagx.Definition() != swagx.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := swagx.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, swagx.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Swag import or Bundle() mutated the autoload composition")
	}
}
