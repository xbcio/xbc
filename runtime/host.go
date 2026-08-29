package runtime

import (
	"context"
	"reflect"

	"github.com/xbcio/xbc/plugin"
)

// hostAdapter is the one and only implementation of plugin.RuntimeHost. It exists
// so that plugin.Context can reach the assembly container's value registry
// and the app's task runtime without the plugin package importing either -- plugin
// is the package everything else depends on, so it can depend on nothing but
// config, log and the standard library.
//
// The adapter deliberately holds *App rather than being *App: plugin.RuntimeHost is
// a four-method port, and having App satisfy it directly would mean every
// Context in the process holds a value that could be type-asserted back to
// the whole application. A plugin that did so would get at the assembly
// container, the settings, the shutdown machinery and the stop channel -- exactly the
// backdoor routing this through a narrow port is meant to close. Asserting
// hostAdapter back to hostAdapter, by contrast, yields nothing but these
// same four methods again.
type hostAdapter struct{ app *App }

// ProvideValue and LookupValue forward straight to the assembly container's
// value registry, which is the (type, instance) store shared by every Context
// belonging to this App.
func (h hostAdapter) ProvideValue(typ reflect.Type, instance string, v any) {
	h.app.container.ProvideValue(typ, instance, v)
}

func (h hostAdapter) LookupValue(typ reflect.Type, instance string) (any, error) {
	return h.app.container.LookupValue(typ, instance)
}

// InitializedPlugins forwards to the assembly container, which refuses to
// answer until every plugin has finished Init. That refusal is what makes
// plugin.Extensions[T] safe to call from inside any plugin's own Init: it
// either sees the complete set or an error, never a snapshot that happens to
// be missing whichever contributors sort after the caller.
func (h hostAdapter) InitializedPlugins() ([]plugin.Extension[any], error) {
	return h.app.container.InitializedPlugins()
}

// GoManaged forwards to the app's task group. The bool submit returns is
// dropped here rather than propagated: plugin.RuntimeHost.GoManaged has no return
// value because Context.Go/GoCritical have none either, and a rejected
// submission is already reported both ways it can usefully be observed --
// logged by submit itself, and available ahead of time through
// Context.TasksAccepted, which reaches the same state via the optional
// reporter interface below.
func (h hostAdapter) GoManaged(id plugin.Identity, fn func(context.Context), critical bool) {
	h.app.tasks.submit(id, fn, critical)
}

// TasksAccepted implements plugin's optional taskAdmissionReporter
// capability, backing Context.TasksAccepted. It is not part of the RuntimeHost
// interface -- RuntimeHost's method set is pinned at four -- so it lives here
// as a plain extra method that plugin discovers by type assertion.
func (h hostAdapter) TasksAccepted() bool {
	return h.app.tasks.accepts()
}
