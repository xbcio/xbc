package csrf_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	csrf "github.com/xbcio/xbc/transport/web/extensions/security/csrf"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if csrf.Definition() == zeroDefinition || csrf.Definition() != csrf.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := csrf.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, csrf.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
