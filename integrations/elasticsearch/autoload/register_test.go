package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/elasticsearch"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresElasticsearchBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Elasticsearch Bundle")
	}
	if !reflect.DeepEqual(got, elasticsearch.Bundle()) {
		t.Fatal("autoload declared composition other than elasticsearch.Bundle()")
	}
}
