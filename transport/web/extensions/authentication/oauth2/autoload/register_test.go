package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
	oauth2web "github.com/xbcio/xbc/transport/web/extensions/authentication/oauth2"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, oauth2web.Bundle()) {
		t.Fatal("autoload declared composition other than oauth2web.Bundle()")
	}
}
