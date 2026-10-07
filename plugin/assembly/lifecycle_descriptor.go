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
	preStopperType    = reflect.TypeOf((*plugin.PreStopper)(nil)).Elem()
	drainerType       = reflect.TypeOf((*plugin.Drainer)(nil)).Elem()

	// lifecycleStages pairs each stage's name with the interface that declares
	// it and with the predicate that reports whether an adapter covers it, so
	// pointer-only detection can name the offending stage -- and can tell a
	// stage it must reject from one an adapter already supplies.
	lifecycleStages = []struct {
		name    string
		iface   reflect.Type
		covered func(pluginmodel.LifecycleAdapters) bool
	}{
		{"Init", initializerType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.Init != nil }},
		{"Migrate", migratorType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.Migrate != nil }},
		{"Start", runnerType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.Start != nil }},
		{"OpenTraffic", trafficOpenerType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.OpenTraffic != nil }},
		{"Stop", closerType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.Stop != nil }},
		{"PreStop", preStopperType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.PreStop != nil }},
		{"Drain", drainerType, func(adapters pluginmodel.LifecycleAdapters) bool { return adapters.Drain != nil }},
	}
)

func compileLifecycle(definition pluginmodel.DefinitionDescriptor) (lifecycleDescriptor, error) {
	if err := rejectPointerOnlyLifecycle(definition); err != nil {
		return lifecycleDescriptor{}, err
	}
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
	// PreStop is compiled last because it was added last, not because it runs
	// last: it is the first hook the runtime invokes during a shutdown. What
	// this branch has to get right is the same thing every branch above does --
	// the adapter and the method are mutually exclusive, so neither can shadow
	// the other -- and the order of the branches only decides which of two
	// ambiguity errors a Definition with several duplicated stages is told
	// about first.
	if primary.Implements(preStopperType) {
		if adapters.PreStop != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "PreStop")
		}
		descriptor.preStop = func(value any, context context.Context) error {
			return value.(plugin.PreStopper).PreStop(context)
		}
	} else if adapters.PreStop != nil {
		descriptor.preStop = adapters.PreStop
	}
	// Drain is compiled last for the same reason PreStop was appended rather
	// than inserted: it was added last, not because it runs last -- it runs
	// after PreStop and before Stop. The mutual-exclusion rule is identical to
	// every stage above.
	if primary.Implements(drainerType) {
		if adapters.Drain != nil {
			return lifecycleDescriptor{}, duplicateLifecycle(definition, "Drain")
		}
		descriptor.drain = func(value any, context context.Context) error {
			return value.(plugin.Drainer).Drain(context)
		}
	} else if adapters.Drain != nil {
		descriptor.drain = adapters.Drain
	}
	return descriptor, nil
}

// rejectPointerOnlyLifecycle refuses a Definition whose primary type reaches a
// lifecycle stage only through its pointer and declares no adapter for that
// stage. The runtime boxes the value the factory returns, and a value's method
// set never contains a method declared with a pointer receiver, so the stage's
// method could never run: every branch below tests the primary type, no branch
// would compile the hook, and the stage would be skipped without a word. An
// adapter for the same stage covers it completely -- the adapter is the hook
// then, and the unreachable method is never consulted -- so only a stage no
// adapter covers is rejected. Whether the author meant to return *T or to
// adapt the stage is theirs to decide; silently skipping the hook is not a
// choice freeze may make.
func rejectPointerOnlyLifecycle(definition pluginmodel.DefinitionDescriptor) error {
	pointer := reflect.PointerTo(definition.Primary)
	for _, stage := range lifecycleStages {
		if definition.Primary.Implements(stage.iface) || !pointer.Implements(stage.iface) {
			continue
		}
		if stage.covered(definition.Lifecycle) {
			continue
		}
		return fmt.Errorf(
			"xbc: plugin %q primary type %s declares the %s lifecycle stage only with a pointer receiver, so the hook would never run: declare *%s as the primary type or add a Lifecycle adapter for that stage",
			definition.Key, definition.Primary, stage.name, definition.Primary)
	}
	return nil
}

func duplicateLifecycle(definition pluginmodel.DefinitionDescriptor, stage string) error {
	return fmt.Errorf("xbc: plugin %q primary type %s implements %s and also declares an adapter for that stage", definition.Key, definition.Primary, stage)
}
