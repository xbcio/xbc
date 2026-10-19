package oauth2_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	oauth2web "github.com/xbcio/xbc/transport/web/extensions/authentication/oauth2"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if oauth2web.Definition() == zeroDefinition || oauth2web.Definition() != oauth2web.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := oauth2web.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, oauth2web.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary oauth2 web import or Bundle() mutated the autoload composition")
	}
}
