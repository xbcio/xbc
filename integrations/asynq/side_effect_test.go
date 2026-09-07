package asynq_test

import (
	"reflect"
	"testing"

	integrationasynq "github.com/xbcio/xbc/integrations/asynq"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if integrationasynq.Definition() != integrationasynq.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = integrationasynq.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Asynq import or Bundle() mutated the autoload composition")
	}
}
