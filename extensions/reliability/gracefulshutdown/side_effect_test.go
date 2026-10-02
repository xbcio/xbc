package gracefulshutdown_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/reliability/gracefulshutdown"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if gracefulshutdown.Definition() == zeroDefinition || gracefulshutdown.Definition() != gracefulshutdown.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := gracefulshutdown.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, gracefulshutdown.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
