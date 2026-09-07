package casbinredis

import (
	"reflect"
	"testing"

	"github.com/alicebob/miniredis/v2"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
	xbccasbin "github.com/xbcio/xbc/transport/web/integrations/casbin"
)

func TestDefinitionIsCanonicalAndExportsWatcherFactory(t *testing.T) {
	first, second := Definition(), Definition()
	if first != second {
		t.Fatal("Definition() returned different declaration handles")
	}
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle()))
	if len(entries) != 1 || !pluginmodel.SameDefinition(entries[0].Definition, pluginmodel.Definition(first)) {
		t.Fatalf("Bundle entries = %+v, want canonical Definition", entries)
	}

	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(first))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) || descriptor.Cardinality != pluginmodel.MultipleInstances {
		t.Fatalf("Definition descriptor = %+v", descriptor)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationConfigured || descriptor.Activation.Path != "plugins.casbin-redis" {
		t.Fatalf("Definition activation = %+v", descriptor.Activation)
	}
	if descriptor.Primary != reflect.TypeOf((*Factory)(nil)) {
		t.Fatalf("Definition primary = %v, want *Factory", descriptor.Primary)
	}
	contract := reflect.TypeOf((*xbccasbin.WatcherFactory)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != contract {
		t.Fatalf("Definition contracts = %+v, want %v", descriptor.Contracts, contract)
	}
	if descriptor.Config == nil || descriptor.Config.Type != reflect.TypeOf(Config{}) {
		t.Fatalf("Definition config = %+v", descriptor.Config)
	}
	if descriptor.Lifecycle.Init != nil || descriptor.Lifecycle.Start != nil || descriptor.Lifecycle.Stop != nil {
		t.Fatalf("Factory unexpectedly owns lifecycle hooks: %+v", descriptor.Lifecycle)
	}
}

func TestNewFactoryDoesNotConnect(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}

	factory, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if factory == nil {
		t.Fatal("New() returned nil Factory")
	}
	if got := server.TotalConnectionCount(); got != 0 {
		t.Fatalf("New() opened %d Redis connections, want 0", got)
	}
}
