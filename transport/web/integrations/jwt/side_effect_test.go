package jwt_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/integrations/jwt"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if jwt.Definition() == zeroDefinition || jwt.Definition() != jwt.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := jwt.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, jwt.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary jwt import or Bundle() mutated the autoload composition")
	}
}
