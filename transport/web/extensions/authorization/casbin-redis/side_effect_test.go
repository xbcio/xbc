package casbinredis_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
	casbinredis "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-redis"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	var zero plugin.Definition
	if casbinredis.Definition() == zero || casbinredis.Definition() != casbinredis.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := casbinredis.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) || !reflect.DeepEqual(bundle, casbinredis.Bundle()) {
		t.Fatal("Bundle() must return stable non-empty composition")
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary casbinredis import or Bundle() mutated autoload composition")
	}
}
