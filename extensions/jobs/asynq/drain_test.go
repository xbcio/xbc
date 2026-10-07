package asynq

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

// TestDrainStopsFetchingTasksAndLeavesShutdownToStop pins the split between
// the two shutdown halves: drain stops the worker pulling new tasks while
// enqueue and Redis stay usable, and stop is the only side that shuts the
// worker down.
func TestDrainStopsFetchingTasksAndLeavesShutdownToStop(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	client := p.client
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if worker.stopCount() != 1 {
		t.Fatalf("worker Stop count after drain = %d, want 1", worker.stopCount())
	}
	if worker.shutdownCount() != 0 {
		t.Fatalf("worker Shutdown count after drain = %d, want 0: Shutdown belongs to stop", worker.shutdownCount())
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); err != nil {
		t.Fatalf("Enqueue after drain error = %v, want enqueue to stay open until Stop", err)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("repeated drain() error = %v", err)
	}
	if worker.stopCount() != 1 {
		t.Fatalf("worker Stop count after repeated drain = %d, want 1", worker.stopCount())
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count after stop = %d, want 1", worker.shutdownCount())
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after stop error = %v, want ErrClosed", err)
	}
}

// TestDrainBeforeStartStopsNothingAndRefusesStart pins the pre-Start path: a
// drain completes with no handlers to wait for, leaves Shutdown to stop, and
// keeps Start refused afterwards.
func TestDrainBeforeStartStopsNothingAndRefusesStart(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() before Start error = %v", err)
	}
	if worker.stopCount() != 1 {
		t.Fatalf("worker Stop count = %d, want 1", worker.stopCount())
	}
	if err := p.start(ctx); err == nil {
		t.Fatal("start() after drain error = nil")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count = %d, want 1", worker.shutdownCount())
	}
}

// TestDrainWhileWaitingForTheTrafficGateKeepsTheWorkerClosed pins the gate
// race: a drain that runs while the worker task still waits for the traffic
// gate leaves it closed for good, so opening the gate afterwards consumes
// nothing.
func TestDrainWhileWaitingForTheTrafficGateKeepsTheWorkerClosed(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	host.openTraffic()
	time.Sleep(20 * time.Millisecond)
	if worker.startCount() != 0 {
		t.Fatal("worker consumed after drain while it was still waiting for the traffic gate")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestDrainWaitsForRunningHandlersWithinItsContext pins that drain returns only
// after the handler invocations already accepted have returned, not just after
// the worker stopped fetching.
func TestDrainWaitsForRunningHandlersWithinItsContext(t *testing.T) {
	redisServer := miniredis.RunT(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var handlerErr error
	handler := HandlerFunc(func(ctx context.Context, _ Task) error {
		once.Do(func() { close(started) })
		<-release
		mu.Lock()
		handlerErr = ctx.Err()
		mu.Unlock()
		return nil
	})
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), handler)
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")

	waitHandler := worker.runCapturedHandler(t)
	<-started
	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while a handler was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if worker.shutdownCount() != 0 {
		t.Fatal("drain shut the worker down instead of waiting for its handlers")
	}
	close(release)
	waitHandler()
	if err := <-drained; err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	mu.Lock()
	got := handlerErr
	mu.Unlock()
	if got != nil {
		t.Fatalf("drain cancelled the running handler: %v", got)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestDrainWithFailedWorkerStopStillWaitsForRunningHandlers pins that a
// panicking worker Stop does not turn drain into a shortcut: drain still waits
// within its context for the handler invocations already accepted, and reports
// the worker failure joined with whatever the wait produced.
func TestDrainWithFailedWorkerStopStillWaitsForRunningHandlers(t *testing.T) {
	redisServer := miniredis.RunT(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := HandlerFunc(func(context.Context, Task) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})
	worker := &fakeWorkerServer{stopPanic: "boom"}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), handler)
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")

	waitHandler := worker.runCapturedHandler(t)
	<-started
	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while a handler was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	waitHandler()
	select {
	case err := <-drained:
		if err == nil || !strings.Contains(err.Error(), "worker stop panicked") {
			t.Fatalf("drain() error = %v, want the joined worker Stop panic", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not return after the handler completed")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestExpiredDrainLeavesHandlersRunningForStop pins the expired-drain rule: an
// expired context means stop waiting, never abort. The handler keeps running
// with a live context, a later drain still waits for it, and stop is the side
// that shuts the worker down.
func TestExpiredDrainLeavesHandlersRunningForStop(t *testing.T) {
	redisServer := miniredis.RunT(t)
	started := make(chan struct{})
	completed := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var once sync.Once
	var mu sync.Mutex
	var handlerErr error
	handler := HandlerFunc(func(ctx context.Context, _ Task) error {
		once.Do(func() { close(started) })
		<-release
		mu.Lock()
		handlerErr = ctx.Err()
		mu.Unlock()
		close(completed)
		return nil
	})
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), handler)
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	defer releaseOnce.Do(func() { close(release) })
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")

	waitHandler := worker.runCapturedHandler(t)
	<-started
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline", err)
	}
	select {
	case <-completed:
		t.Fatal("expired drain cancelled the handler")
	default:
	}

	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("later drain returned while the handler was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	waitHandler()
	<-completed
	if err := <-drained; err != nil {
		t.Fatalf("later drain() error = %v", err)
	}
	mu.Lock()
	got := handlerErr
	mu.Unlock()
	if got != nil {
		t.Fatalf("expired drain cancelled the running handler: %v", got)
	}
	if worker.shutdownCount() != 0 {
		t.Fatal("drain shut the worker down instead of leaving it for stop")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count after stop = %d, want 1", worker.shutdownCount())
	}
}

// TestDrainWaitsForRunningHandlersToComplete runs a real worker whose handler
// outlives the configured shutdown timeout. A drain with a live context must
// wait for the handler instead of letting the library abort it, and the task
// must complete normally once the handler returns.
func TestDrainWaitsForRunningHandlersToComplete(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Concurrency = 1
	cfg.ShutdownTimeout = 100 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var startedOnce sync.Once
	var mu sync.Mutex
	var handlerErr error
	p := newTestPlugin(t, cfg, HandlerFunc(func(ctx context.Context, _ Task) error {
		startedOnce.Do(func() { close(started) })
		<-release
		mu.Lock()
		handlerErr = ctx.Err()
		mu.Unlock()
		return nil
	}))
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	client := p.client
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.stop(stopCtx)
		host.stopTasks()
	}()
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start the handler")
	}

	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while the handler was still running: %v", err)
	case <-time.After(3 * cfg.ShutdownTimeout):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not return after the handler completed")
	}
	mu.Lock()
	got := handlerErr
	mu.Unlock()
	if got != nil {
		t.Fatalf("drain cancelled the running handler: %v", got)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestDrainDoesNotCancelHandlersThatOutliveItsBudget runs a real worker whose
// handler outlives both the drain context and the configured shutdown timeout.
// Had drain started the library's Shutdown, the handler would be aborted and
// its task requeued; drain must instead leave it running with an untouched
// context for stop.
func TestDrainDoesNotCancelHandlersThatOutliveItsBudget(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Concurrency = 1
	cfg.ShutdownTimeout = 100 * time.Millisecond
	started := make(chan struct{})
	completed := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var startedOnce sync.Once
	var mu sync.Mutex
	var handlerErr error
	p := newTestPlugin(t, cfg, HandlerFunc(func(ctx context.Context, _ Task) error {
		startedOnce.Do(func() { close(started) })
		<-release
		mu.Lock()
		handlerErr = ctx.Err()
		mu.Unlock()
		close(completed)
		return nil
	}))
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	client := p.client
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.stop(stopCtx)
		host.stopTasks()
	}()
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start the handler")
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline", err)
	}
	// A drain that wrongly started the library's Shutdown would abort the
	// handler after its whole timeout.
	select {
	case <-completed:
		t.Fatal("handler completed before it was released")
	case <-time.After(3 * cfg.ShutdownTimeout):
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not complete after it was released")
	}
	mu.Lock()
	got := handlerErr
	mu.Unlock()
	if got != nil {
		t.Fatalf("expired drain cancelled the running handler: %v", got)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestStopWaitsForHandlersAnExpiredDrainLeftBehind pins the other half of the
// expired-drain rule: the handler drain left running is waited for by stop
// instead of being cut off by the worker shutdown, and Redis closes only after
// it returned.
func TestStopWaitsForHandlersAnExpiredDrainLeftBehind(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Concurrency = 1
	cfg.ShutdownTimeout = 2 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var startedOnce sync.Once
	var mu sync.Mutex
	var handlerErr error
	p := newTestPlugin(t, cfg, HandlerFunc(func(ctx context.Context, _ Task) error {
		startedOnce.Do(func() { close(started) })
		<-release
		mu.Lock()
		handlerErr = ctx.Err()
		mu.Unlock()
		return nil
	}))
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	client := p.client
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.stop(stopCtx)
		host.stopTasks()
	}()
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start the handler")
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- p.stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while the handler the drain left was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not return after the handler completed")
	}
	mu.Lock()
	got := handlerErr
	mu.Unlock()
	if got != nil {
		t.Fatalf("stop cancelled the handler the drain left: %v", got)
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after stop error = %v, want ErrClosed", err)
	}
}
