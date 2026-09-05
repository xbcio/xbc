package objectstorage_test

import (
	"testing"

	"github.com/xbcio/xbc/integrations/objectstorage"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/internal/pluginmodel"
)

func TestOrdinaryImportHasNoAutoloadSideEffect(t *testing.T) {
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(objectstorage.Bundle()))
	if len(entries) != 1 {
		t.Fatalf("objectstorage.Bundle() entries = %d, want 1", len(entries))
	}
	descriptor, ok := pluginmodel.DescribeDefinition(entries[0].Definition)
	if !ok || descriptor.Key != pluginmodel.Key(objectstorage.Key) {
		t.Fatalf("objectstorage Bundle descriptor = %+v, found = %v", descriptor, ok)
	}

	global := pluginmodel.BundleEntries(pluginmodel.Bundle(internalautoload.Freeze()))
	if len(global) != 0 {
		t.Fatalf("ordinary package import declared %d global bundle entries", len(global))
	}
}
