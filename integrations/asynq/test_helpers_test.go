package asynq

import (
	"context"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type testHost struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	accepting bool
	tasks     sync.WaitGroup
	submitted int
	critical  int
}

func newTestHost() *testHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &testHost{
		ctx:       ctx,
		cancel:    cancel,
		gate:      make(chan struct{}),
		accepting: true,
	}
}

func (h *testHost) ExecutionContext() context.Context { return h.ctx }
func (*testHost) Logger() corelog.Logger              { return corelog.Nop() }
func (h *testHost) TrafficGate() <-chan struct{}      { return h.gate }
func (*testHost) RequestShutdown(plugin.Identity, string) bool {
	return false
}

func (h *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), critical bool) bool {
	h.mu.Lock()
	if !h.accepting {
		h.mu.Unlock()
		return false
	}
	h.submitted++
	if critical {
		h.critical++
	}
	h.tasks.Add(1)
	ctx := h.ctx
	h.mu.Unlock()
	go func() {
		defer h.tasks.Done()
		fn(ctx)
	}()
	return true
}

func (h *testHost) setTasksAccepted(accepting bool) {
	h.mu.Lock()
	h.accepting = accepting
	h.mu.Unlock()
}

func (h *testHost) counts() (submitted, critical int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitted, h.critical
}

func (h *testHost) openTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *testHost) stopTasks() {
	h.cancel()
	h.tasks.Wait()
}

func testContext(host plugin.RuntimeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

func validTestConfig(addr string) Config {
	cfg := defaultConfig()
	cfg.Redis.Addr = addr
	cfg.Redis.MaxRetries = -1
	cfg.Concurrency = 2
	cfg.TaskCheckInterval = 10 * time.Millisecond
	cfg.ShutdownTimeout = 2 * time.Second
	return cfg
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timeout waiting for %s", description)
	}
}

func redisClientClosed(client *goredis.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return client.Ping(ctx).Err() != nil
}
