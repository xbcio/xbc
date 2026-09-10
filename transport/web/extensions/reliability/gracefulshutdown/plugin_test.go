package gracefulshutdown

import (
	"context"
	"sync"
	"testing"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type testHost struct {
	mu sync.Mutex

	calls     int
	identity  plugin.Identity
	reason    string
	stopOnce  sync.Once
	execution context.Context
	cancel    context.CancelFunc
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	execution, cancel := context.WithCancel(context.Background())
	return &testHost{execution: execution, cancel: cancel}
}

func (h *testHost) ExecutionContext() context.Context { return h.execution }
func (*testHost) Logger() log.Logger                  { return log.Nop() }
func (*testHost) TrafficGate() <-chan struct{}        { return nil }
func (*testHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}

func (h *testHost) RequestShutdown(identity plugin.Identity, reason string) bool {
	accepted := false
	h.stopOnce.Do(func() {
		accepted = true
		h.mu.Lock()
		h.calls++
		h.identity = identity
		h.reason = reason
		h.mu.Unlock()
		h.cancel()
	})
	return accepted
}

func initializedController(t *testing.T) (*Controller, *testHost) {
	t.Helper()
	controller := New()
	host := newTestHost()
	ctx := plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key})
	if err := controller.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return controller, host
}

func initializedPlugin(t *testing.T, mutate func(*Config)) (*Plugin, *testHost) {
	t.Helper()
	cfg := defaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	controller, host := initializedController(t)
	p, err := newPlugin(cfg, controller)
	if err != nil {
		t.Fatal(err)
	}
	return p, host
}

func TestNewControllerUsesRuntimeShutdownPath(t *testing.T) {
	controller, host := initializedController(t)
	if !controller.Request(" operator\nrequest ") {
		t.Fatal("first shutdown request was not accepted")
	}
	if controller.Request("again") {
		t.Fatal("second shutdown request was accepted")
	}
	if !controller.ShuttingDown() {
		t.Fatal("controller did not observe lifecycle cancellation")
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if host.calls != 1 || host.identity.Plugin != Key || host.reason != "operator request" {
		t.Fatalf("host shutdown = calls %d, identity %v, reason %q", host.calls, host.identity, host.reason)
	}
}

func TestControllerInitRequiresContext(t *testing.T) {
	if err := New().Init(nil); err == nil {
		t.Fatal("Init(nil) succeeded")
	}
}

func TestStopDetachesController(t *testing.T) {
	controller, _ := initializedController(t)
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.Request("after stop") {
		t.Fatal("detached controller accepted shutdown")
	}
}

func TestNewPluginPropagatesCustomEndpointPath(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.HTTP.Enabled = true
		cfg.HTTP.Path = "/ops/shutdown"
	})
	if p.endpoint.path != "/ops/shutdown" {
		t.Fatalf("endpoint path = %q, want the configured plugins.gracefulshutdown.http.path", p.endpoint.path)
	}
}

func TestNewPluginUsesProvidedController(t *testing.T) {
	controller := New()
	cfg := defaultConfig()
	cfg.HTTP.Enabled = true
	p, err := newPlugin(cfg, controller)
	if err != nil {
		t.Fatal(err)
	}
	if p.controller != controller || !p.endpoint.enabled {
		t.Fatalf("plugin = %#v, want enabled adapter for provided controller", p)
	}
}
