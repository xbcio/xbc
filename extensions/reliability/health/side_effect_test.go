package health_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if health.Definition() == zeroDefinition || health.Definition() != health.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := health.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, health.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
