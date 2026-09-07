package accesslog_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	accesslogplugin "github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if accesslogplugin.Definition() == zeroDefinition || accesslogplugin.Definition() != accesslogplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := accesslogplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, accesslogplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
