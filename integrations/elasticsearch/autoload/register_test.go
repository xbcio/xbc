package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/elasticsearch"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresElasticsearchBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Elasticsearch Bundle")
	}
	if !reflect.DeepEqual(got, elasticsearch.Bundle()) {
		t.Fatal("autoload declared composition other than elasticsearch.Bundle()")
	}
}
