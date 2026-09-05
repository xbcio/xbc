package elasticsearch_test

import (
	"reflect"
	"testing"

	integration "github.com/xbcio/xbc/integrations/elasticsearch"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
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
