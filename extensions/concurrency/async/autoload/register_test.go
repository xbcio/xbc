package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/concurrency/async"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresAsyncBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the async Bundle")
	}
	if !reflect.DeepEqual(got, async.Bundle()) {
		t.Fatal("autoload declared composition other than async.Bundle()")
	}
}
