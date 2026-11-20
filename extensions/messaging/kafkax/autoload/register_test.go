package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/messaging/kafkax"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresKafkaBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Kafka Bundle")
	}
	if !reflect.DeepEqual(got, kafkax.Bundle()) {
		t.Fatal("autoload declared composition other than kafkax.Bundle()")
	}
}
