package swag_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/openapi/swag"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if swag.Definition() == zeroDefinition || swag.Definition() != swag.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := swag.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, swag.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Swag import or Bundle() mutated the autoload composition")
	}
}
