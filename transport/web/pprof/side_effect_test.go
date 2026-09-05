package pprof_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/pprof"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if pprof.Definition() == zeroDefinition || pprof.Definition() != pprof.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := pprof.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, pprof.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
