package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
)

func TestImportDeclaresCanonicalBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare a Bundle")
	}
	if !reflect.DeepEqual(got, tracing.Bundle()) {
		t.Fatal("autoload declared composition other than tracing.Bundle()")
	}
}
