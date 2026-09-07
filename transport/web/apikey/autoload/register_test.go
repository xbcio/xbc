package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/apikey"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, apikey.Bundle()) {
		t.Fatal("autoload declared composition other than apikey.Bundle()")
	}
}
