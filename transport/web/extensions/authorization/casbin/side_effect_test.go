package casbin_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if casbin.Definition() == zeroDefinition || casbin.Definition() != casbin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := casbin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, casbin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary casbin import or Bundle() mutated the autoload composition")
	}
}
