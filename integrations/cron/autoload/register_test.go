package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/cron"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresCronBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the cron Bundle")
	}
	if !reflect.DeepEqual(got, cron.Bundle()) {
		t.Fatal("autoload declared composition other than cron.Bundle()")
	}
}
