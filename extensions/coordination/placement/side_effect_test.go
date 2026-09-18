package placement_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/coordination/placement"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

// TestOrdinaryImportAndBundleHaveNoAutoloadSideEffect keeps this module's
// Bundle usable as ordinary composition data. A Bundle accessor that mutated
// the process-wide default composition would make selecting this plugin a
// decision taken by an import statement, and the root facade's explicit
// composition would silently carry an extra plugin.
func TestOrdinaryImportAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if placement.Definition() != placement.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = placement.Bundle()

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary import or Bundle() mutated the default composition")
	}
}
