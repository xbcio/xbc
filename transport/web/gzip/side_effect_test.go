package gzip_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	gzipplugin "github.com/xbcio/xbc/transport/web/gzip"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if gzipplugin.Definition() == zeroDefinition || gzipplugin.Definition() != gzipplugin.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := gzipplugin.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, gzipplugin.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
