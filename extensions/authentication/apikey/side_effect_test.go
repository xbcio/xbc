package apikey_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if apikey.Definition() == zeroDefinition || apikey.Definition() != apikey.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := apikey.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, apikey.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary apikey import or Bundle() mutated the autoload composition")
	}
}
