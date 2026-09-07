package redis_test

import (
	"reflect"
	"testing"

	integrationredis "github.com/xbcio/xbc/extensions/storage/redis"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if integrationredis.Definition() != integrationredis.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = integrationredis.Bundle()

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Redis import or Bundle() mutated the default composition")
	}
}
