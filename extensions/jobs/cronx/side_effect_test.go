package cronx_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/jobs/cronx"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if cronx.Definition() != cronx.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = cronx.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary cron import or Bundle() mutated the autoload composition")
	}
}
