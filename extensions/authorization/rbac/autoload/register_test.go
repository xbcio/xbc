package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, rbac.Bundle()) {
		t.Fatal("autoload declared composition other than rbac.Bundle()")
	}
}
