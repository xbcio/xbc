package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/jwtx"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, jwtx.Bundle()) {
		t.Fatal("autoload declared composition other than jwt.Bundle()")
	}
}
