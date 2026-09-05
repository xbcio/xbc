package cron_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/cron"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
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
