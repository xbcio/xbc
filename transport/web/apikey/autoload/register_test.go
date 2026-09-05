package autoload

import (
	"reflect"
	"testing"

	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/apikey"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, apikey.Bundle()) {
		t.Fatal("autoload declared composition other than apikey.Bundle()")
	}
}
