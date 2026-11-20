package mailx_test

import (
	"reflect"
	"testing"

	integrationmail "github.com/xbcio/xbc/extensions/messaging/mailx"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if integrationmail.Definition() != integrationmail.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = integrationmail.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary mail import or Bundle() mutated the autoload composition")
	}
}
