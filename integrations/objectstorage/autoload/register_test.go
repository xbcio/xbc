package autoload

import (
	"testing"

	"github.com/xbcio/xbc/integrations/objectstorage"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestImportDeclaresOnlyObjectStorageBundle(t *testing.T) {
	bundle := pluginautoload.Freeze()
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	if len(entries) != 1 {
		t.Fatalf("autoload bundle entries = %d, want 1", len(entries))
	}
	canonical := pluginmodel.Definition(objectstorage.Definition())
	if !pluginmodel.SameDefinition(entries[0].Definition, canonical) {
		t.Fatal("autoload did not declare object storage's canonical Definition")
	}
	descriptor, ok := pluginmodel.DescribeDefinition(entries[0].Definition)
	if !ok || descriptor.Key != pluginmodel.Key(objectstorage.Key) {
		t.Fatalf("autoload Definition descriptor = %+v, found = %v", descriptor, ok)
	}
}
