package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/kafka"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresKafkaBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Kafka Bundle")
	}
	if !reflect.DeepEqual(got, kafka.Bundle()) {
		t.Fatal("autoload declared composition other than kafka.Bundle()")
	}
}
