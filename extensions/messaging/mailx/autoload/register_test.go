package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/messaging/mailx"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresMailBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the mail Bundle")
	}
	if !reflect.DeepEqual(got, mailx.Bundle()) {
		t.Fatal("autoload declared composition other than mailx.Bundle()")
	}
}
