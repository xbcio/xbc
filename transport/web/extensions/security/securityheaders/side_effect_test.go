package securityheaders_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	securityheadersplugin "github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if securityheadersplugin.Definition() == zeroDefinition || securityheadersplugin.Definition() != securityheadersplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := securityheadersplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, securityheadersplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
