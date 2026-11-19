package banner_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/observability/banner"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

// TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect is the guard
// against moving composition into import time: evaluating the package's
// declarations must not join the process-wide autoload composition.
func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zero plugin.Definition
	if banner.Definition() == zero || banner.Definition() != banner.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := banner.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, banner.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if defaults := banner.DefaultConfig(); defaults.Style != banner.StyleASCII || defaults.Font != "standard" || defaults.Text != "" {
		t.Fatalf("DefaultConfig() = %#v, want the documented defaults", defaults)
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary import or Bundle() mutated the autoload composition")
	}
}
