package asynqx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"

	corelog "github.com/xbcio/xbc/log"
)

func TestRetryAfterIsRecognizedThroughWrapping(t *testing.T) {
	cause := errors.New("temporary failure")
	wrapped := fmt.Errorf("handler: %w", RetryAfter(cause, 90*time.Second))

	var delayer RetryDelayer
	if !errors.As(wrapped, &delayer) {
		t.Fatal("errors.As did not recognize a wrapped RetryAfter")
	}
	if delayer.RetryDelay() != 90*time.Second {
		t.Fatalf("RetryDelay() = %s, want 90s", delayer.RetryDelay())
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("the cause is no longer reachable through errors.Is")
	}
	if _, ok := retryDelay(wrapped); !ok {
		t.Fatal("retryDelay did not report the embedded delay")
	}

	if err := RetryAfter(nil, time.Second); err == nil {
		t.Fatal("RetryAfter(nil, ...) = nil, want an error so the task is not treated as successful")
	} else if delay, ok := retryDelay(err); !ok || delay != time.Second {
		t.Fatalf("RetryAfter(nil, 1s) = %v/%v, want a 1s deferral", delay, ok)
	}
	if delay, ok := retryDelay(RetryAfter(cause, -time.Second)); !ok || delay != 0 {
		t.Fatalf("negative delay = %v/%v, want zero", delay, ok)
	}
	if _, ok := retryDelay(cause); ok {
		t.Fatal("a plain error reported a retry delay")
	}
}

func TestRetryDelayFuncPrefersSelfReportedDelay(t *testing.T) {
	fn := newRetryDelayFunc(time.Second, 60*time.Second)
	reported := fmt.Errorf("wrapped: %w", RetryAfter(errors.New("defer"), 11*time.Second))
	if got := fn(0, reported, nil); got != 11*time.Second {
		t.Fatalf("reported delay = %s, want 11s", got)
	}
	// A self-reported delay is not capped by the backoff cap.
	if got := fn(0, RetryAfter(errors.New("defer"), 10*time.Minute), nil); got != 10*time.Minute {
		t.Fatalf("reported delay above the cap = %s, want 10m", got)
	}
	if got := fn(0, errors.New("boom"), nil); got != time.Second {
		t.Fatalf("backoff at attempt 0 = %s, want 1s", got)
	}
}

func TestExponentialBackoffDefaultsAndCap(t *testing.T) {
	fn := newRetryDelayFunc(time.Second, 60*time.Second)
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: time.Second},
		{attempt: 1, want: 2 * time.Second},
		{attempt: 3, want: 8 * time.Second},
		{attempt: 5, want: 32 * time.Second},
		{attempt: 6, want: 60 * time.Second},
		{attempt: 100, want: 60 * time.Second},
		{attempt: -1, want: time.Second},
	} {
		if got := fn(test.attempt, errors.New("boom"), nil); got != test.want {
			t.Fatalf("backoff(attempt=%d) = %s, want %s", test.attempt, got, test.want)
		}
	}

	configured := newRetryDelayFunc(2*time.Second, 10*time.Second)
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 2 * time.Second},
		{attempt: 2, want: 8 * time.Second},
		{attempt: 3, want: 10 * time.Second},
	} {
		if got := configured(test.attempt, errors.New("boom"), nil); got != test.want {
			t.Fatalf("configured backoff(attempt=%d) = %s, want %s", test.attempt, got, test.want)
		}
	}

	if got := exponentialBackoff(1<<20, time.Second, 60*time.Second); got != 60*time.Second {
		t.Fatalf("huge attempt backoff = %s, want the cap", got)
	}
}

func TestConfigRetryBackoffDefaultsBindingAndValidation(t *testing.T) {
	cfg := bindAsynqConfig(t, map[string]any{})
	if cfg.RetryBackoffBase != time.Second || cfg.RetryBackoffCap != 60*time.Second {
		t.Fatalf("retry backoff defaults = %s/%s, want 1s/60s", cfg.RetryBackoffBase, cfg.RetryBackoffCap)
	}

	bound := bindAsynqConfig(t, map[string]any{
		"retry_backoff_base": "3s",
		"retry_backoff_cap":  "2m",
	})
	if bound.RetryBackoffBase != 3*time.Second || bound.RetryBackoffCap != 2*time.Minute {
		t.Fatalf("bound retry backoff = %s/%s, want 3s/2m", bound.RetryBackoffBase, bound.RetryBackoffCap)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "zero base", mutate: func(c *Config) { c.RetryBackoffBase = 0 }, want: "retry_backoff_base"},
		{name: "zero cap", mutate: func(c *Config) { c.RetryBackoffCap = 0 }, want: "retry_backoff_cap"},
		{name: "cap below base", mutate: func(c *Config) { c.RetryBackoffCap = time.Millisecond }, want: "must be at least"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInitInstallsRetryDelayPolicyOnEveryWorker(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.RetryBackoffBase = 3 * time.Second
	cfg.RetryBackoffCap = 45 * time.Second
	p := newTestPlugin(t, cfg, HandlerFunc(func(context.Context, Task) error { return nil }))
	captured := make([]hibiken.Config, 0, len(p.groups))
	p.factory.newServer = func(_ goredis.UniversalClient, workerConfig hibiken.Config) workerServer {
		captured = append(captured, workerConfig)
		return &fakeWorkerServer{}
	}
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	if len(captured) == 0 {
		t.Fatal("init() built no worker")
	}
	for index, workerConfig := range captured {
		if workerConfig.RetryDelayFunc == nil || workerConfig.ErrorHandler == nil {
			t.Fatalf("worker %d retry policy = %+v", index, workerConfig)
		}
		if got := workerConfig.RetryDelayFunc(0, errors.New("boom"), nil); got != 3*time.Second {
			t.Fatalf("worker %d first backoff = %s, want 3s", index, got)
		}
		if got := workerConfig.RetryDelayFunc(6, errors.New("boom"), nil); got != 45*time.Second {
			t.Fatalf("worker %d capped backoff = %s, want 45s", index, got)
		}
	}
}

func TestTaskErrorHandlerLogsClusterLimitAtWarn(t *testing.T) {
	logger := &levelRecorder{}
	handler := newTaskErrorHandler(logger)
	task := hibiken.NewTask("work", nil)

	handler.HandleError(context.Background(), task, RetryAfter(fmt.Errorf("deferred: %w", ErrClusterConcurrencyLimit), time.Second))
	if len(logger.warns) != 1 || len(logger.errors) != 0 || len(logger.infos) != 0 {
		t.Fatalf("cluster-limit logging = warn:%v error:%v info:%v", logger.warns, logger.errors, logger.infos)
	}

	handler.HandleError(context.Background(), task, RetryAfter(errors.New("recheck later"), time.Minute))
	if len(logger.warns) != 1 || len(logger.infos) != 1 {
		t.Fatalf("deferral logging = warn:%v error:%v info:%v", logger.warns, logger.errors, logger.infos)
	}

	handler.HandleError(context.Background(), task, errors.New("plain failure"))
	if len(logger.warns) != 1 || len(logger.infos) != 1 || len(logger.errors) != 0 {
		t.Fatalf("plain failure logging = warn:%v error:%v info:%v", logger.warns, logger.errors, logger.infos)
	}
}

// levelRecorder records messages per level so a test can pin which level a
// failure is reported at.
type levelRecorder struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errors []string
}

func (l *levelRecorder) Debug(string, ...any) {}
func (l *levelRecorder) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *levelRecorder) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *levelRecorder) Error(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}
func (*levelRecorder) Fatal(string, ...any) { panic("unexpected fatal") }
func (l *levelRecorder) With(...any) corelog.Logger {
	return l
}
func (*levelRecorder) Enabled(corelog.Level) bool { return true }

// deliveryRecord is what one handler invocation observed on its Task.
type deliveryRecord struct {
	retried    int
	maxRetries int
	taskType   string
	payload    string
}

// TestTaskRetryMetadata drives a real worker through one failed
// attempt and its redelivery, checking that the Task a handler receives carries
// this delivery's retry state rather than a zero value in every field.
func TestTaskRetryMetadata(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Concurrency = 1
	// A near-zero backoff makes the scheduled retry immediate, but the queue
	// library's forwarder still polls on its own interval, so the waits below
	// are generous rather than tight.
	cfg.RetryBackoffBase = 10 * time.Millisecond
	cfg.RetryBackoffCap = 10 * time.Millisecond

	deliveries := make(chan deliveryRecord, 2)
	var attempts atomic.Int32
	handler := HandlerFunc(func(_ context.Context, task Task) error {
		deliveries <- deliveryRecord{
			retried:    task.Retried,
			maxRetries: task.MaxRetries,
			taskType:   task.Type,
			payload:    string(task.Payload),
		}
		if attempts.Add(1) == 1 {
			return errors.New("first attempt fails so the worker retries")
		}
		return nil
	})

	p := newTestPlugin(t, cfg, handler)
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	host.openTraffic()

	if _, err := p.Enqueue(context.Background(), Task{Type: "work", Payload: []byte("retry-payload")}, MaxRetries(1)); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	first := waitForDelivery(t, deliveries, "first")
	if first.retried != 0 {
		t.Fatalf("first delivery Retried = %d, want 0", first.retried)
	}
	if first.maxRetries != 1 {
		t.Fatalf("first delivery MaxRetries = %d, want 1", first.maxRetries)
	}
	if first.taskType != "work" || first.payload != "retry-payload" {
		t.Fatalf("first delivery = %q/%q, want work/retry-payload", first.taskType, first.payload)
	}

	second := waitForDelivery(t, deliveries, "retried")
	if second.retried != 1 {
		t.Fatalf("retried delivery Retried = %d, want 1", second.retried)
	}
	if second.maxRetries != 1 {
		t.Fatalf("retried delivery MaxRetries = %d, want 1", second.maxRetries)
	}
	if second.taskType != "work" || second.payload != "retry-payload" {
		t.Fatalf("retried delivery = %q/%q, want work/retry-payload", second.taskType, second.payload)
	}
}

// waitForDelivery returns the next handler observation or fails the test. The
// redelivery wait spans the queue library's forwarder interval, which is not
// configurable through this package.
func waitForDelivery(t *testing.T, deliveries <-chan deliveryRecord, attempt string) deliveryRecord {
	t.Helper()
	select {
	case record := <-deliveries:
		return record
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for the %s delivery; the worker did not redeliver the task", attempt)
		return deliveryRecord{}
	}
}
