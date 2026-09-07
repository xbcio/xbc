package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/asynq"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresAsynqBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Asynq Bundle")
	}
	if !reflect.DeepEqual(got, asynq.Bundle()) {
		t.Fatal("autoload declared composition other than asynq.Bundle()")
	}
}
