package web_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if web.Definition() == zeroDefinition || web.Definition() != web.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	if reflect.DeepEqual(web.Bundle(), plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(web.Bundle(), web.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
