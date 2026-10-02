package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/reliability/gracefulshutdown"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, gracefulshutdown.Bundle()) {
		t.Fatal("autoload declared composition other than gracefulshutdown.Bundle()")
	}
}
