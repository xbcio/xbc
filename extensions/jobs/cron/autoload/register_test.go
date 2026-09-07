package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/jobs/cron"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresCronBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the cron Bundle")
	}
	if !reflect.DeepEqual(got, cron.Bundle()) {
		t.Fatal("autoload declared composition other than cron.Bundle()")
	}
}
