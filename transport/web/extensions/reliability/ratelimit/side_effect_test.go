package ratelimit_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	ratelimit "github.com/xbcio/xbc/transport/web/extensions/reliability/ratelimit"
)

func TestDefinitionAndBundleAreCanonical(t *testing.T) {
	var zeroDefinition plugin.Definition
	if ratelimit.Definition() == zeroDefinition || ratelimit.Definition() != ratelimit.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := ratelimit.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, ratelimit.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

}
