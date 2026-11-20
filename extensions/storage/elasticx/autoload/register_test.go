package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/storage/elasticx"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresElasticsearchBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Elasticsearch Bundle")
	}
	if !reflect.DeepEqual(got, elasticx.Bundle()) {
		t.Fatal("autoload declared composition other than elasticx.Bundle()")
	}
}
