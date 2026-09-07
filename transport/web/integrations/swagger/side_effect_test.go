package swagger_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/integrations/swagger"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if swagger.Definition() == zeroDefinition || swagger.Definition() != swagger.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := swagger.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, swagger.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Swagger import or Bundle() mutated the autoload composition")
	}
}
