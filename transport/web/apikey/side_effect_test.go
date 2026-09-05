package apikey_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/apikey"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if apikey.Definition() == zeroDefinition || apikey.Definition() != apikey.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := apikey.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, apikey.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
