package async

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type fakeHost struct {
	ctx context.Context
	log log.Logger
}

func newFakeHost() *fakeHost { return &fakeHost{ctx: context.Background(), log: log.Nop()} }

func (host *fakeHost) ExecutionContext() context.Context {
	if host == nil || host.ctx == nil {
		return context.Background()
	}
	return host.ctx
}

func (host *fakeHost) Logger() log.Logger {
	if host == nil || host.log == nil {
		return log.Nop()
	}
	return host.log
}
func (*fakeHost) ProcessInstance() string                                      { return "test-process" }
func (*fakeHost) TrafficGate() <-chan struct{}                                 { gate := make(chan struct{}); close(gate); return gate }
func (*fakeHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool { return false }
func (*fakeHost) RequestShutdown(plugin.Identity, string) bool                 { return false }

func runtimeContext(host plugin.RuntimeHost, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: instance})
}

// newTestPool builds a Pool directly (bypassing Definition/Init) with the
// given config, for tests that exercise Pool behavior without a global
// binding.
func newTestPool(t *testing.T, cfg Config) *Pool {
	t.Helper()
	pool, err := newPreparedPool(cfg, log.Nop())
	if err != nil {
		t.Fatalf("newPreparedPool: %v", err)
	}
	return pool
}

// mustNewPool builds an unopened Pool directly, for tests that call open,
// drain, or Init themselves rather than going through newPreparedPool.
func mustNewPool(t *testing.T, cfg Config, logger log.Logger) *Pool {
	t.Helper()
	pool, err := newPool(cfg, logger)
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	return pool
}

// executorConfigs names one base Config per executor, used by the shared
// behavior suite (executor_shared_test.go) to run the same assertions
// against both goroutineExecutor and antsExecutor. Every entry starts from
// DefaultConfig so a test that overrides MaxConcurrency/QueueCapacity/etc.
// still gets each executor's own required settings (ExecutorAnts needs a
// positive MaxConcurrency) without repeating them at every call site.
func executorConfigs() map[string]Config {
	goroutineCfg := DefaultConfig()
	goroutineCfg.Executor = ExecutorGoroutine

	antsCfg := DefaultConfig()
	antsCfg.Executor = ExecutorAnts

	return map[string]Config{
		ExecutorGoroutine: goroutineCfg,
		ExecutorAnts:      antsCfg,
	}
}

// waitForCondition polls condition until it is true or the deadline passes.
func waitForCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// blockingTask returns a task function that signals started, then blocks
// until release is closed or its context is cancelled, recording completion
// (and whether it was cancelled) on finished.
type blockingTask struct {
	started  chan struct{}
	release  chan struct{}
	finished chan error
}

func newBlockingTask() *blockingTask {
	return &blockingTask{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan error, 1),
	}
}

func (b *blockingTask) run(ctx context.Context) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	var err error
	select {
	case <-b.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	select {
	case b.finished <- err:
	default:
	}
}

// counter is a small concurrency-safe counter for tests.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
