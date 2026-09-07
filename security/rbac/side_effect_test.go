package rbac_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/security/rbac"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if rbac.Definition() == zeroDefinition || rbac.Definition() != rbac.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := rbac.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) || !reflect.DeepEqual(bundle, rbac.Bundle()) {
		t.Fatal("Bundle() must return stable non-empty composition")
	}
}
