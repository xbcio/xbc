package assembly

import (
	"context"
	"fmt"
	"reflect"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

var (
	initializerType   = reflect.TypeOf((*plugin.Initializer)(nil)).Elem()
	migratorType      = reflect.TypeOf((*plugin.Migrator)(nil)).Elem()
	runnerType        = reflect.TypeOf((*plugin.Runner)(nil)).Elem()
	trafficOpenerType = reflect.TypeOf((*plugin.TrafficOpener)(nil)).Elem()
	closerType        = reflect.TypeOf((*plugin.Closer)(nil)).Elem()
)

func compileLifecycle(definition pluginmodel.DefinitionDescriptor) (lifecycleDescriptor, error) {
	var descriptor lifecycleDescriptor
	primary := definition.Primary
	adapters := definition.Lifecycle

	if primary.Implements(initializerType) {
		if adapters.Init != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "Init")
		}
		descriptor.init = func(value any, context *plugin.Context) error {
			return value.(plugin.Initializer).Init(context)
		}
	} else if adapters.Init != nil {
		descriptor.init = func(value any, context *plugin.Context) error {
			return adapters.Init(value, context)
		}
	}
	if primary.Implements(migratorType) {
		if adapters.Migrate != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "Migrate")
		}
		descriptor.migrate = func(value any, context *plugin.Context) error {
			return value.(plugin.Migrator).Migrate(context)
		}
	} else if adapters.Migrate != nil {
		descriptor.migrate = func(value any, context *plugin.Context) error {
			return adapters.Migrate(value, context)
		}
	}
	if primary.Implements(runnerType) {
		if adapters.Start != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "Start")
		}
		descriptor.start = func(value any, context *plugin.Context) error {
			return value.(plugin.Runner).Start(context)
		}
	} else if adapters.Start != nil {
		descriptor.start = func(value any, context *plugin.Context) error {
			return adapters.Start(value, context)
		}
	}
	if primary.Implements(trafficOpenerType) {
		if adapters.OpenTraffic != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "OpenTraffic")
		}
		descriptor.openTraffic = func(value any, context *plugin.Context) error {
			return value.(plugin.TrafficOpener).OpenTraffic(context)
		}
	} else if adapters.OpenTraffic != nil {
		descriptor.openTraffic = func(value any, context *plugin.Context) error {
			return adapters.OpenTraffic(value, context)
		}
	}
	if primary.Implements(closerType) {
		if adapters.Stop != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "Stop")
		}
		descriptor.stop = func(value any, context context.Context) error {
			return value.(plugin.Closer).Stop(context)
		}
	} else if adapters.Stop != nil {
		descriptor.stop = adapters.Stop
	}
	return descriptor, nil
}

func duplicateLifecycle(definition pluginmodel.DefinitionDescriptor, stage string) error {
	return fmt.Errorf("xbc: plugin %q primary type %s implements %s and also declares an adapter for that stage", definition.Key, definition.Primary, stage)
}
