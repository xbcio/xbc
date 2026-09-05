package web

import (
	"context"
	"sync"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type submittedTask struct {
	identity plugin.Identity
	critical bool
}

// fakeHost is the smallest faithful RuntimeHost needed by Web lifecycle tests.
// It owns one traffic gate and one cancellable task scope, just like runtime.
type fakeHost struct {
	mu sync.Mutex

	execution context.Context
	logger    log.Logger
	gate      chan struct{}
	gateOnce  sync.Once

	acceptTasks bool
	taskContext context.Context
	cancelTasks context.CancelFunc
	tasks       []submittedTask
	wg          sync.WaitGroup

	shutdownRequested bool
	shutdownIdentity  plugin.Identity
	shutdownReason    string
}

var _ plugin.RuntimeHost = (*fakeHost)(nil)

func newFakeHost() *fakeHost {
	execution, cancel := context.WithCancel(context.Background())
	return &fakeHost{
		execution:   execution,
		logger:      log.Nop(),
		gate:        make(chan struct{}),
		acceptTasks: true,
		taskContext: execution,
		cancelTasks: cancel,
	}
}

func (h *fakeHost) ExecutionContext() context.Context { return h.execution }
func (h *fakeHost) Logger() log.Logger                { return h.logger }
func (h *fakeHost) TrafficGate() <-chan struct{}      { return h.gate }

func (h *fakeHost) SubmitTask(id plugin.Identity, fn func(context.Context), critical bool) bool {
	if fn == nil {
		return false
	}
	h.mu.Lock()
	if !h.acceptTasks {
		h.mu.Unlock()
		return false
	}
	h.tasks = append(h.tasks, submittedTask{identity: id.Normalized(), critical: critical})
	h.wg.Add(1)
	taskContext := h.taskContext
	h.mu.Unlock()

	go func() {
		defer h.wg.Done()
		fn(taskContext)
	}()
	return true
}

func (h *fakeHost) RequestShutdown(id plugin.Identity, reason string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shutdownRequested {
		return false
	}
	h.shutdownRequested = true
	h.shutdownIdentity = id.Normalized()
	h.shutdownReason = reason
	return true
}

func (h *fakeHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *fakeHost) trafficReleased() bool {
	select {
	case <-h.gate:
		return true
	default:
		return false
	}
}

func (h *fakeHost) rejectTasks() {
	h.mu.Lock()
	h.acceptTasks = false
	h.mu.Unlock()
}

func (h *fakeHost) submittedTasks() []submittedTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]submittedTask(nil), h.tasks...)
}

func (h *fakeHost) shutdown() {
	h.cancelTasks()
	h.wg.Wait()
}

func contextFromHost(host *fakeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}
