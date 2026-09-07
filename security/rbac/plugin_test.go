package rbac

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
