package timeout_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	timeoutplugin "github.com/xbcio/xbc/transport/web/timeout"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if timeoutplugin.Definition() == zeroDefinition || timeoutplugin.Definition() != timeoutplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := timeoutplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, timeoutplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
