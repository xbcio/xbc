package asynq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
