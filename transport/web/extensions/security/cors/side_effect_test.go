package cors_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	cors "github.com/xbcio/xbc/transport/web/extensions/security/cors"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if cors.Definition() == zeroDefinition || cors.Definition() != cors.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := cors.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, cors.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
