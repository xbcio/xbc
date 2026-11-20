package jwtx_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/jwtx"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if jwtx.Definition() == zeroDefinition || jwtx.Definition() != jwtx.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := jwtx.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, jwtx.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary jwt import or Bundle() mutated the autoload composition")
	}
}
