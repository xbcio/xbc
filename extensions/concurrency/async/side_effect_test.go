package async_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/concurrency/async"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if async.Definition() == zeroDefinition || async.Definition() != async.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := async.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, async.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
