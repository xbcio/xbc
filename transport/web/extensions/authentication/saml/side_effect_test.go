package saml_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	samlweb "github.com/xbcio/xbc/transport/web/extensions/authentication/saml"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if samlweb.Definition() == zeroDefinition || samlweb.Definition() != samlweb.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := samlweb.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, samlweb.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary saml web import or Bundle() mutated the autoload composition")
	}
}
