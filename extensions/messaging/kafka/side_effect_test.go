package kafka_test

import (
	"reflect"
	"testing"

	integrationkafka "github.com/xbcio/xbc/extensions/messaging/kafka"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if integrationkafka.Definition() != integrationkafka.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = integrationkafka.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Kafka import or Bundle() mutated the autoload composition")
	}
}
