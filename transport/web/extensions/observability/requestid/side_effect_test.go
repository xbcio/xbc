package requestid_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	requestidplugin "github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if requestidplugin.Definition() == zeroDefinition || requestidplugin.Definition() != requestidplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := requestidplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, requestidplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
