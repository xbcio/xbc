package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
	oidcweb "github.com/xbcio/xbc/transport/web/extensions/authentication/oidc"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, oidcweb.Bundle()) {
		t.Fatal("autoload declared composition other than oidcweb.Bundle()")
	}
}
