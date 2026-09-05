package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/asynq"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresAsynqBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Asynq Bundle")
	}
	if !reflect.DeepEqual(got, asynq.Bundle()) {
		t.Fatal("autoload declared composition other than asynq.Bundle()")
	}
}
