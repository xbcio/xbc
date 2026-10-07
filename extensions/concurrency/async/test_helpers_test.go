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

// capturedEntry is one log entry a captureLogger recorded.
type capturedEntry struct {
	level  string
	msg    string
	fields []any
}

// captureLogger records every entry's level, message, and key/value fields so
// tests can assert on what the Pool logged.
type captureLogger struct {
	mu      sync.Mutex
	entries []capturedEntry
}

func (l *captureLogger) add(level, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, capturedEntry{level: level, msg: msg, fields: append([]any(nil), kv...)})
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) log.Logger      { return l }
func (*captureLogger) Enabled(log.Level) bool        { return true }

// warnFields returns the key/value pairs of the first recorded warning with
// the given message.
func (l *captureLogger) warnFields(msg string) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.level != "warn" || entry.msg != msg {
			continue
		}
		fields := make(map[string]any, len(entry.fields)/2)
		for index := 0; index+1 < len(entry.fields); index += 2 {
			if key, ok := entry.fields[index].(string); ok {
				fields[key] = entry.fields[index+1]
			}
		}
		return fields, true
	}
	return nil, false
}
