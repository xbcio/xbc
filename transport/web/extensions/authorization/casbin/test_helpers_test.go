package casbin

import (
	"context"
	"sync"
	"testing"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// testHost is a minimal plugin.RuntimeHost sufficient to exercise start/Stop
// in isolation from the real runtime.
type testHost struct {
	taskCtx context.Context
	cancel  context.CancelFunc
	tasks   sync.WaitGroup
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &testHost{taskCtx: ctx, cancel: cancel}
}

func (h *testHost) ExecutionContext() context.Context { return h.taskCtx }

func (*testHost) Logger() corelog.Logger  { return corelog.Nop() }
func (*testHost) ProcessInstance() string { return "test-process" }

func (h *testHost) TrafficGate() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

func (h *testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (h *testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }

func (h *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.taskCtx)
	}()
	return true
}

func (*testHost) RequestShutdown(plugin.Identity, string) bool { return true }

func (h *testHost) close() {
	h.cancel()
	h.tasks.Wait()
}

func testContext(host plugin.RuntimeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: "default"})
}

// initializedPlugin builds an engine from engineMutate, wraps it in the
// middleware built from httpMutate, starts the middleware against a fresh
// testHost, and registers cleanup for both. The engine is returned so tests
// can assert on what the middleware did or did not do to it.
func initializedPlugin(t *testing.T, engineMutate func(*casbincore.Config), httpMutate func(*Config), options ...Option) (*Plugin, *casbincore.Plugin) {
	t.Helper()

	engineCfg := casbincore.DefaultConfig()
	if engineMutate != nil {
		engineMutate(&engineCfg)
	}
	engine, err := casbincore.New(engineCfg)
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })

	cfg := DefaultConfig()
	if httpMutate != nil {
		httpMutate(&cfg)
	}
	p, err := New(engine, cfg, options...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		host.close()
	})
	return p, engine
}
