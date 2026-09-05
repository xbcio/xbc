package recovery_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	recoveryplugin "github.com/xbcio/xbc/transport/web/recovery"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if recoveryplugin.Definition() == zeroDefinition || recoveryplugin.Definition() != recoveryplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := recoveryplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, recoveryplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
