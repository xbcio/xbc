package grpc

import (
	"context"
	"sync"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// fakeHost is the smallest faithful RuntimeHost the gRPC lifecycle tests need.
// It owns one traffic gate and one cancellable task scope, just like runtime.
type fakeHost struct {
	mu sync.Mutex

	execution context.Context
	logger    log.Logger
	gate      chan struct{}
	gateOnce  sync.Once

	acceptTasks bool
	cancelTasks context.CancelFunc
	tasks       int
	wg          sync.WaitGroup
}

var _ plugin.RuntimeHost = (*fakeHost)(nil)

func newFakeHost() *fakeHost {
	execution, cancel := context.WithCancel(context.Background())
	return &fakeHost{
		execution:   execution,
		logger:      log.Nop(),
		gate:        make(chan struct{}),
		acceptTasks: true,
		cancelTasks: cancel,
	}
}

func (h *fakeHost) ExecutionContext() context.Context                               { return h.execution }
func (h *fakeHost) Logger() log.Logger                                              { return h.logger }
func (*fakeHost) ProcessInstance() string                                           { return "test-process" }
func (h *fakeHost) TrafficGate() <-chan struct{}                                    { return h.gate }
func (*fakeHost) Admission(plugin.Identity) plugin.Admission                        { return nil }
func (*fakeHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*fakeHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (h *fakeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.mu.Lock()
	if !h.acceptTasks {
		h.mu.Unlock()
		return false
	}
	h.tasks++
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		fn(h.execution)
	}()
	return true
}

func (h *fakeHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *fakeHost) rejectTasks() {
	h.mu.Lock()
	h.acceptTasks = false
	h.mu.Unlock()
}

func (h *fakeHost) submitted() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tasks
}

// shutdown cancels the task scope and waits for every task to return.
func (h *fakeHost) shutdown() {
	h.cancelTasks()
	h.wg.Wait()
}

func contextFromHost(host *fakeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}
