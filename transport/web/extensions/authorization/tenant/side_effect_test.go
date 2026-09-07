package tenant_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/extensions/authorization/tenant"
)

func TestOrdinaryImportHasNoRegistrationSideEffect(t *testing.T) {
	var zero plugin.Definition
	if tenant.Definition() == zero || tenant.Definition() != tenant.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := tenant.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, tenant.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
