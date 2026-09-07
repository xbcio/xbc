package cron_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/jobs/cron"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if cron.Definition() != cron.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = cron.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary cron import or Bundle() mutated the autoload composition")
	}
}
