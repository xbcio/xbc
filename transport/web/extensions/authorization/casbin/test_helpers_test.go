package casbin

import (
	"context"
	"sync"
	"testing"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// testHost is a minimal plugin.RuntimeHost sufficient to exercise start/Stop
// and the managed periodic reload task in isolation from the real runtime.
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

func (*testHost) Logger() corelog.Logger { return corelog.Nop() }

func (h *testHost) TrafficGate() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

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

// initializedPlugin constructs a Plugin from DefaultConfig (optionally
// mutated), starts it against a fresh testHost, and registers cleanup.
func initializedPlugin(t *testing.T, mutate func(*Config), options ...Option) (*Plugin, *testHost) {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, options...)
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
	return p, host
}
