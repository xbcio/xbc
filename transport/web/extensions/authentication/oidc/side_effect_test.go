package oidc_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	oidcweb "github.com/xbcio/xbc/transport/web/extensions/authentication/oidc"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if oidcweb.Definition() == zeroDefinition || oidcweb.Definition() != oidcweb.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := oidcweb.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, oidcweb.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary oidc web import or Bundle() mutated the autoload composition")
	}
}
