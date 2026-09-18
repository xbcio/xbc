package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/coordination/placement"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresPlacementBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the placement Bundle")
	}
	if !reflect.DeepEqual(got, placement.Bundle()) {
		t.Fatal("autoload declared composition other than placement.Bundle()")
	}
}
