package session_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/session"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if session.Definition() == zeroDefinition || session.Definition() != session.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := session.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, session.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary session import or Bundle() mutated the autoload composition")
	}
}
