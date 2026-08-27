// fakehost_test.go
package web

import (
	"context"
	"reflect"
	"sync"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// fakeHost is a minimal, in-memory plugin.RuntimeHost used only by web's own tests.
// web cannot import any of core's internal/* packages (design §9 guard #10)
// nor reach into plugin's own package-private fakehost_test.go, so
// exercising (*Server).Start/OpenTraffic against real
// plugin.Context/Extensions machinery needs a local stand-in that implements
// RuntimeHost's four methods directly, built for web's own tests only.
type fakeHost struct {
	mu sync.Mutex

	initialized    []plugin.Extension[any]
	initializedErr error
}

var _ plugin.RuntimeHost = (*fakeHost)(nil)

func newFakeHost(extensions ...plugin.Extension[any]) *fakeHost {
	return &fakeHost{initialized: extensions}
}

func (h *fakeHost) ProvideValue(reflect.Type, string, any) {}

func (h *fakeHost) LookupValue(typ reflect.Type, instance string) (any, error) {
	return nil, &plugin.NotFoundError{Want: typ, Instance: instance}
}

func (h *fakeHost) InitializedPlugins() ([]plugin.Extension[any], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.initializedErr != nil {
		return nil, h.initializedErr
	}
	out := make([]plugin.Extension[any], len(h.initialized))
	copy(out, h.initialized)
	return out, nil
}

// GoManaged spawns a genuine goroutine, unlike plugin's own package-private
// fakeHost (plugin/fakehost_test.go), which calls fn synchronously because
// that package's tests only ever check that GoManaged was *called*, not
// that it behaves like a real scheduler. web's readiness tests depend on
// OpenTraffic returning immediately while srv.Serve keeps running in the
// background (design §5.1 rule 5) -- a synchronous fn call here would make
// OpenTraffic itself block forever on Serve and the whole point of the
// two-phase contract would go untested.
func (h *fakeHost) GoManaged(_ plugin.Identity, fn func(context.Context), _ bool) {
	go fn(context.Background())
}

// contextFromHost builds a *plugin.Context wired to host, with a discard
// logger and an empty read-only config.View -- everything
// (*Server).Start/OpenTraffic need to run for real, without pulling in the
// real container. The concrete Environment below is used only to construct
// that view; NewRuntimeContext exposes it through the config.View contract.
func contextFromHost(host *fakeHost) *plugin.Context {
	env, err := config.NewEnvironment(nil, "")
	if err != nil {
		panic(err)
	}
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: "web", Instance: "default"}, env, log.Nop())
}

// asAny upcasts a concrete capability value plus its providing Identity
// into the plugin.Extension[any] shape (*fakeHost).initialized holds, so a
// test can hand Start a MiddlewareProvider/RouteProvider/RouteCatalogConsumer
// fixture through the exact same Extensions[T] path Start itself uses.
func asAny(id plugin.Identity, v any) plugin.Extension[any] {
	return plugin.Extension[any]{Identity: id, Value: v}
}
