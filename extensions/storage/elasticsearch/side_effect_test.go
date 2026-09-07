package elasticsearch_test

import (
	"reflect"
	"testing"

	integration "github.com/xbcio/xbc/extensions/storage/elasticsearch"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if integration.Definition() != integration.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = integration.Bundle()

	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Elasticsearch import or Bundle() mutated autoload composition")
	}
}
