package asynq

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/plugin"
)

func testHandlerEntries(handler Handler) []plugin.Entry[HandlerContributor] {
	return []plugin.Entry[HandlerContributor]{{
		Identity: plugin.Identity{Plugin: "business"},
		Value: &testContributor{registrations: []HandlerRegistration{{
			Type: "work", Handler: handler,
		}}},
	}}
}

func newTestPlugin(t *testing.T, cfg Config, handler Handler) *Plugin {
	t.Helper()
	p, err := newPlugin(cfg, testHandlerEntries(handler))
	if err != nil {
		t.Fatalf("newPlugin() error = %v", err)
	}
	return p
}

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestPrepareConfigPreservesDefaultsAndRejectsInvalidValues(t *testing.T) {
	defaults := defaultConfig()
	prepared, err := prepareConfig(defaults)
	if err != nil {
		t.Fatalf("prepareConfig(defaults) error = %v", err)
	}
	if !reflect.DeepEqual(prepared, defaults) {
		t.Fatalf("prepared defaults = %#v, want %#v", prepared, defaults)
	}
	invalid := defaults
	invalid.Concurrency = 0
	if _, err := prepareConfig(invalid); err == nil {
		t.Fatal("prepareConfig(invalid) error = nil")
	}
}

func TestInitPingsBuildsBackendsAndMapsConfig(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"critical": 7, "default": 2}
	cfg.DefaultQueue = "critical"
	cfg.StrictPriority = true
	cfg.Concurrency = 6
	p := newTestPlugin(t, cfg, HandlerFunc(func(context.Context, Task) error { return nil }))
	worker := &fakeWorkerServer{}
	var captured hibiken.Config
	p.factory.newServer = func(_ goredis.UniversalClient, cfg hibiken.Config) workerServer {
		captured = cfg
		return worker
	}
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	if redisServer.CommandCount() == 0 {
		t.Fatal("init() did not ping Redis")
	}
	client := p.client
	if client == nil {
		t.Fatal("init() did not retain its enqueue client")
	}
	if captured.Concurrency != 6 || !captured.StrictPriority || captured.TaskCheckInterval != cfg.TaskCheckInterval || captured.ShutdownTimeout != cfg.ShutdownTimeout {
		t.Fatalf("worker config = %+v", captured)
	}
	if !reflect.DeepEqual(captured.Queues, map[string]int{"critical": 7, "default": 2}) {
		t.Fatalf("worker queues = %v", captured.Queues)
	}
	p.cfg.Queues["critical"] = 99
	if captured.Queues["critical"] != 7 {
		t.Fatalf("worker queue configuration aliased plugin state: %v", captured.Queues)
	}
	if client.defaults.queue != "critical" || client.defaults.maxRetries != cfg.DefaultMaxRetries || client.defaults.timeout != cfg.DefaultTimeout {
		t.Fatalf("client defaults = %+v", client.defaults)
	}
	if err := p.init(testContext(host)); err == nil {
		t.Fatal("second init() error = nil")
	}
}

func TestInitFailureRollsBackRedisAndLeavesPartialStateStoppable(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisServer.SetError("LOADING unavailable")
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	var created *goredis.Client
	p.factory.newRedis = func(options *goredis.Options) *goredis.Client {
		created = goredis.NewClient(options)
		return created
	}
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err == nil {
		t.Fatal("init() error = nil")
	}
	if created == nil || !redisClientClosed(created) {
		t.Fatal("failed init did not close its Redis client")
	}
	if p.initialized || p.redis != nil || p.client != nil || p.server != nil {
		t.Fatalf("failed init retained state: %+v", p)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop after failed init error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("repeated stop after failed init error = %v", err)
	}
}

func TestStartSubmitsCriticalWorkerThatWaitsForTrafficGate(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if submitted, critical := host.counts(); submitted != 1 || critical != 1 {
		t.Fatalf("managed tasks = %d critical = %d, want 1/1", submitted, critical)
	}
	time.Sleep(20 * time.Millisecond)
	if worker.startCount() != 0 {
		t.Fatal("worker consumed before the runtime traffic gate opened")
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")
	if err := p.start(ctx); err == nil {
		t.Fatal("second start() error = nil")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	host.stopTasks()
}

func TestStartHandlesManagedTaskRejectionAndStopIsSafe(t *testing.T) {
	redisServer := miniredis.RunT(t)
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return &fakeWorkerServer{} }
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	host.setTasksAccepted(false)
	if err := p.start(ctx); err == nil {
		t.Fatal("start() with rejected task error = nil")
	}
	if submitted, critical := host.counts(); submitted != 0 || critical != 0 {
		t.Fatalf("rejected start admitted %d/%d tasks", submitted, critical)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop after rejected start error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("idempotent stop after rejected start error = %v", err)
	}
	host.stopTasks()
}

func TestRealWorkerWaitsForGateAndStopDrains(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Concurrency = 1
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := newTestPlugin(t, cfg, HandlerFunc(func(context.Context, Task) error {
		once.Do(func() { close(started) })
		<-release
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
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}, TaskID("work-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("task processed before traffic gate")
	case <-time.After(75 * time.Millisecond):
	}
	host.openTraffic()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		_ = p.stop(context.Background())
		host.stopTasks()
		t.Fatal("worker did not process task")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- p.stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("stop returned before handler drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after stop error = %v", err)
	}
	host.stopTasks()
}

func TestConcurrentStopSharesCleanupAndHonorsWaitingCallerContext(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{shutdownStarted: make(chan struct{}), shutdownRelease: make(chan struct{})}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- p.stop(context.Background()) }()
	<-worker.shutdownStarted
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.stop(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting stop error = %v, want deadline", err)
	}
	close(worker.shutdownRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first stop error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("completed stop error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count = %d", worker.shutdownCount())
	}
}

func TestStopBeforeInitIsIdempotentAndShutdownPanicBecomesError(t *testing.T) {
	p := newTestPlugin(t, defaultConfig(), HandlerFunc(func(context.Context, Task) error { return nil }))
	if err := p.stop(nil); err != nil {
		t.Fatalf("stop before init error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("repeated stop error = %v", err)
	}
	if err := p.init(testContext(newTestHost())); err == nil {
		t.Fatal("init after stop error = nil")
	}
	panicWorker := &fakeWorkerServer{shutdownPanic: "boom"}
	if err := shutdownWorker(panicWorker); err == nil {
		t.Fatal("shutdownWorker panic error = nil")
	}
}

type fakeWorkerServer struct {
	mu              sync.Mutex
	handler         hibiken.Handler
	starts          int
	startErr        error
	shutdowns       int
	shutdownStarted chan struct{}
	shutdownRelease chan struct{}
	shutdownPanic   any
	startOnce       sync.Once
}

func (s *fakeWorkerServer) Start(handler hibiken.Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	s.handler = handler
	return s.startErr
}

func (s *fakeWorkerServer) Shutdown() {
	s.mu.Lock()
	s.shutdowns++
	started := s.shutdownStarted
	release := s.shutdownRelease
	panicValue := s.shutdownPanic
	s.mu.Unlock()
	if started != nil {
		s.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	if panicValue != nil {
		panic(panicValue)
	}
}

func (s *fakeWorkerServer) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func (s *fakeWorkerServer) shutdownCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdowns
}
