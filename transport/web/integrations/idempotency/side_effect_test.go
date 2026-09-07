package idempotency_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/integrations/idempotency"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zeroDefinition plugin.Definition
	if idempotency.Definition() == zeroDefinition || idempotency.Definition() != idempotency.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := idempotency.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, idempotency.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary idempotency import or Bundle() mutated the autoload composition")
	}
}
