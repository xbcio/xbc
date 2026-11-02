package redissyncmap

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	xbcconfig "github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	redisstorage "github.com/xbcio/xbc/extensions/storage/redis"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

// TestAssembledPluginExportsTheFactory resolves the exported contract through
// the real graph: a consumer asks the configured redis-syncmap plugin for a
// map and receives a working one.
func TestAssembledPluginExportsTheFactory(t *testing.T) {
	server := miniredis.RunT(t)

	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis": map[string]any{
				"cache": map[string]any{"addr": server.Addr()},
			},
			"redis-syncmap": map[string]any{
				"default": map[string]any{
					"redis_instance": "cache",
					"namespace":      "assembly",
				},
			},
		},
	}, "XBC_REDIS_SYNCMAP_ASSEMBLY_TEST_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	factoryInput := plugin.RefTo[syncmap.Factory](Key)
	consumerDefinition := plugin.Define(
		"syncmap-consumer",
		func(ctx plugin.BuildContext) (*consumer, error) {
			rules, err := factoryInput.Get(ctx).Value.Map("rules",
				syncmap.WithTransform(func(_ string, raw json.RawMessage) (any, error) {
					var record testRecord
					if err := json.Unmarshal(raw, &record); err != nil {
						return nil, err
					}
					return record, nil
				}),
			)
			if err != nil {
				return nil, err
			}
			return &consumer{rules: rules}, nil
		},
		plugin.Options[*consumer]{Inputs: plugin.Inputs(factoryInput)},
	)

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(consumerDefinition), Bundle(), redisstorage.Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}
	host := newFakeHost()
	t.Cleanup(func() {
		host.cancel()
		host.join()
	})
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: "syncmap-consumer", Instance: plugin.DefaultInstance}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
	})

	instance, ok := constructed.Instance(plugin.Identity{Plugin: "syncmap-consumer", Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the consumer must be constructed")
	}
	built, ok := instance.Primary().(*consumer)
	if !ok {
		t.Fatalf("consumer primary = %T, want *consumer", instance.Primary())
	}
	if built.rules == nil {
		t.Fatal("the consumer did not receive a map from the exported factory")
	}
}

type consumer struct {
	rules syncmap.Map
}
