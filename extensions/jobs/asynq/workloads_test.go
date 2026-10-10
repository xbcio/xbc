package asynq

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/plugin"
)

// TestInitBuildsOneWorkerPerWorkload is the core of the split: a process that
// hosts several workloads runs one worker per workload, each with the queues
// and concurrency that workload declared.
func TestInitBuildsOneWorkerPerWorkload(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	cfg.Concurrency = 4
	cfg.Workloads = map[string]WorkloadConfig{
		"sast": {Queues: map[string]int{"sast": 1}, Concurrency: 2},
		"saas": {Queues: map[string]int{"saas": 3}},
	}
	p := newWorkloadPlugin(t, cfg,
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
		contributorEntry("saas", "saas", "saas.report"),
	)
	var built []hibiken.Config
	p.factory.newServer = func(_ goredis.UniversalClient, workerConfig hibiken.Config) workerServer {
		built = append(built, workerConfig)
		return &fakeWorkerServer{}
	}

	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })

	if len(built) != 3 {
		t.Fatalf("built %d workers, want one per group: %+v", len(built), built)
	}
	wantQueues := []map[string]int{{"default": 1}, {"sast": 1}, {"saas": 3}}
	wantConcurrency := []int{4, 2, 4}
	for index, workerConfig := range built {
		if !reflect.DeepEqual(workerConfig.Queues, wantQueues[index]) {
			t.Fatalf("worker %d queues = %v, want %v", index, workerConfig.Queues, wantQueues[index])
		}
		// A workload that declares no concurrency of its own takes the
		// top-level one; declaring one overrides it.
		if workerConfig.Concurrency != wantConcurrency[index] {
			t.Fatalf("worker %d concurrency = %d, want %d", index, workerConfig.Concurrency, wantConcurrency[index])
		}
	}
}

// TestWorkerGroupsChargeTheWorkloadTheirHandlersBelongTo pins down what a
// shared worker plugin asks the runtime for: its own identity for the
// contributors that belong to no workload, and the contributor's workload for
// every workload group -- the same quota the workload's managed tasks charge.
func TestWorkerGroupsChargeTheWorkloadTheirHandlersBelongTo(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	cfg.Workloads = map[string]WorkloadConfig{
		"sast": {Queues: map[string]int{"sast": 1}},
		"saas": {Queues: map[string]int{"saas": 1}},
	}
	p := newWorkloadPlugin(t, cfg,
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
		contributorEntry("saas", "saas", "saas.report"),
	)
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return &fakeWorkerServer{} }

	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })

	want := []admissionQuery{
		{identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}},
		{identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, workload: "sast", shared: true},
		{identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, workload: "saas", shared: true},
	}
	if got := host.queries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("admission queries = %+v, want %+v", got, want)
	}
}

func TestAssemblePluginRefusesAWorkloadThatDeclaresNoQueues(t *testing.T) {
	cfg := defaultConfig()
	_, err := assemblePlugin(cfg, []plugin.Entry[HandlerContributor]{
		contributorEntry("sast", "sast", "sast.scan"),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "plugins.asynq.workloads.sast") {
		t.Fatalf("assemblePlugin() error = %v, want the missing workload entry named", err)
	}
}

func TestAssemblePluginRefusesAQueueTwoWorkersWouldBothConsume(t *testing.T) {
	cfg := defaultConfig()
	cfg.Queues = map[string]int{"shared": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"shared": 2}}}
	_, err := assemblePlugin(cfg, []plugin.Entry[HandlerContributor]{
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
	}, nil)
	if err == nil {
		t.Fatal("assemblePlugin() error = nil")
	}
	for _, want := range []string{`queue "shared"`, `the unowned group`, `workload "sast"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("assemblePlugin() error = %v, want %q named", err, want)
		}
	}
}

func TestWorkloadsWithoutContributorsEverywhereIsNotAConflict(t *testing.T) {
	// The same queue name in two processes that host different workloads is a
	// deployment decision, so it must not be refused: the conflict is only
	// between workers of one process, and a workload that contributes nothing
	// here has no worker here.
	cfg := defaultConfig()
	cfg.Queues = map[string]int{"housekeeping": 1}
	cfg.Workloads = map[string]WorkloadConfig{
		"sast": {Queues: map[string]int{"sast": 1}},
	}
	p := newWorkloadPlugin(t, cfg, contributorEntry("housekeeping", "", "housekeeping.sweep"))
	if len(p.groups) != 1 || p.groups[0].workload != "" {
		t.Fatalf("groups = %v, want only the unowned group", p.groups)
	}
}

// TestStartRunsOneWorkerPerGroupBehindTheTrafficGate checks that each group
// gets its own managed task, that no worker polls Redis before the traffic
// gate opens, and that each worker serves only its own group's task types.
func TestStartRunsOneWorkerPerGroupBehindTheTrafficGate(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	sastErr := errors.New("sast handler ran")
	p := newWorkloadPlugin(t, cfg,
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
	)
	p.groups[1].dispatcher.handlers["sast.scan"] = HandlerFunc(func(context.Context, Task) error { return sastErr })

	var servers []*fakeWorkerServer
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer {
		server := &fakeWorkerServer{}
		servers = append(servers, server)
		return server
	}

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

	if submitted, critical := host.counts(); submitted != 2 || critical != 2 {
		t.Fatalf("managed tasks = %d critical = %d, want one critical task per group", submitted, critical)
	}
	time.Sleep(20 * time.Millisecond)
	for index, server := range servers {
		if server.startCount() != 0 {
			t.Fatalf("worker %d polled Redis before the traffic gate opened", index)
		}
	}

	host.openTraffic()
	for _, server := range servers {
		waitForCondition(t, 2*time.Second, func() bool { return server.startCount() == 1 }, "the worker to open")
	}

	unowned := servers[0].deliveredHandler()
	sast := servers[1].deliveredHandler()
	if unowned == nil || sast == nil {
		t.Fatal("a worker was started without a handler")
	}
	if err := sast.ProcessTask(context.Background(), hibiken.NewTask("sast.scan", nil)); !errors.Is(err, sastErr) {
		t.Fatalf("sast worker delivered its own type with error %v", err)
	}
	if err := sast.ProcessTask(context.Background(), hibiken.NewTask("housekeeping.sweep", nil)); !errors.Is(err, ErrHandlerNotFound) {
		t.Fatalf("sast worker served another group's type with error %v", err)
	}
	if err := unowned.ProcessTask(context.Background(), hibiken.NewTask("housekeeping.sweep", nil)); err != nil {
		t.Fatalf("unowned worker did not serve its own type: %v", err)
	}
	if err := unowned.ProcessTask(context.Background(), hibiken.NewTask("sast.scan", nil)); !errors.Is(err, ErrHandlerNotFound) {
		t.Fatalf("unowned worker served a workload's type with error %v", err)
	}
}

// TestHandlerHoldsItsWorkloadsQuotaForTheWholeInvocation checks the accounting
// the split exists for: every delivery charges one unit of its workload's
// quota, and the unit is held until the handler returns.
func TestHandlerHoldsItsWorkloadsQuotaForTheWholeInvocation(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	events := &eventLog{}
	admission := &recordingAdmission{events: events}
	p := newWorkloadPlugin(t, cfg,
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
	)
	p.groups[1].dispatcher.handlers["sast.scan"] = HandlerFunc(func(context.Context, Task) error {
		events.add("handler")
		return nil
	})
	server := &fakeWorkerServer{}
	var serverIndex int
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer {
		serverIndex++
		if serverIndex == 2 {
			return server
		}
		return &fakeWorkerServer{}
	}

	host := newTestHost()
	host.admissionForWorkload = func(plugin.WorkloadKey) plugin.Admission { return admission }
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
	waitForCondition(t, 2*time.Second, func() bool { return server.startCount() == 1 }, "the workload worker to open")

	if err := server.deliveredHandler().ProcessTask(context.Background(), hibiken.NewTask("sast.scan", nil)); err != nil {
		t.Fatalf("delivery error = %v", err)
	}
	if got, want := events.list(), []string{"acquire", "handler", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if admission.counts() != (chargeCounts{acquired: 1, released: 1}) {
		t.Fatalf("charges = %+v, want one unit taken and given back", admission.counts())
	}
}

// TestDeliveryThatCannotTakeItsQuotaIsReportedAsAnError covers the wait ending
// without a slot: the task must come back to the queue as a failure the caller
// can see, never be silently dropped.
func TestDeliveryThatCannotTakeItsQuotaIsReportedAsAnError(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	deadline := errors.New("no quota before the task deadline")
	ran := false
	p := newWorkloadPlugin(t, cfg,
		contributorEntry("housekeeping", "", "housekeeping.sweep"),
		contributorEntry("sast", "sast", "sast.scan"),
	)
	p.groups[1].dispatcher.handlers["sast.scan"] = HandlerFunc(func(context.Context, Task) error {
		ran = true
		return nil
	})
	server := &fakeWorkerServer{}
	var serverIndex int
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer {
		serverIndex++
		if serverIndex == 2 {
			return server
		}
		return &fakeWorkerServer{}
	}
	host := newTestHost()
	host.admissionForWorkload = func(plugin.WorkloadKey) plugin.Admission {
		return admissionFunc(func(context.Context) (func(), error) { return nil, deadline })
	}
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
	waitForCondition(t, 2*time.Second, func() bool { return server.startCount() == 1 }, "the workload worker to open")

	err := server.deliveredHandler().ProcessTask(context.Background(), hibiken.NewTask("sast.scan", nil))
	if !errors.Is(err, deadline) {
		t.Fatalf("delivery error = %v, want the admission error", err)
	}
	if ran {
		t.Fatal("the handler ran without its workload's quota")
	}
}

// TestUnownedWorkerChargesNoQuota keeps the two halves honest: a contributor
// that belongs to no workload is bounded by its worker's concurrency alone,
// exactly as Context.Admission answers for an unowned plugin, and the delivery
// still runs even though there is no budget to charge.
func TestUnownedWorkerChargesNoQuota(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Queues = map[string]int{"default": 1}
	events := &eventLog{}
	p := newWorkloadPlugin(t, cfg, contributorEntry("housekeeping", "", "housekeeping.sweep"))
	p.groups[0].dispatcher.handlers["housekeeping.sweep"] = HandlerFunc(func(context.Context, Task) error {
		events.add("handler")
		return nil
	})
	server := &fakeWorkerServer{}
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return server }

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
	waitForCondition(t, 2*time.Second, func() bool { return server.startCount() == 1 }, "the worker to open")

	if err := server.deliveredHandler().ProcessTask(context.Background(), hibiken.NewTask("housekeeping.sweep", nil)); err != nil {
		t.Fatalf("delivery error = %v", err)
	}
	if got, want := events.list(), []string{"handler"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for _, query := range host.queries() {
		if query.shared {
			t.Fatalf("unowned worker asked for a workload's quota: %+v", query)
		}
	}
}

// TestProcessWithoutHandlersIdlesInsteadOfRefusing covers the standby case: a
// process that contributes no handlers still starts, still provides a
// long-lived task so the runtime can hand it to its signal loop, and consumes
// nothing from Redis.
func TestProcessWithoutHandlersIdlesInsteadOfRefusing(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	built := 0
	p, err := assemblePlugin(cfg, nil, nil)
	if err != nil {
		t.Fatalf("assemblePlugin() with no contributors error = %v", err)
	}
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer {
		built++
		return &fakeWorkerServer{}
	}

	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if built != 0 {
		t.Fatalf("built %d workers, want none without contributors", built)
	}
	submitted, critical := host.counts()
	if submitted != 1 || critical != 1 {
		t.Fatalf("managed tasks = %d critical = %d, want one critical task that waits for the stop", submitted, critical)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	// The idle task ends with the process rather than holding it open.
	stopped := make(chan struct{})
	go func() {
		host.stopTasks()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the idle provider task did not return when its context ended")
	}
}

// TestEnqueueAcceptsQueuesDeclaredForAnyWorkload keeps enqueue validation on
// the whole declared vocabulary. A process that only enqueues work for a
// workload it does not host is a normal deployment, so its client must accept
// that workload's queues while still refusing names nothing declared.
func TestEnqueueAcceptsQueuesDeclaredForAnyWorkload(t *testing.T) {
	cfg := defaultConfig()
	cfg.Queues = map[string]int{"default": 1}
	cfg.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	backend := &countingEnqueueBackend{}
	client := newClient(backend, cfg)

	if _, err := client.Enqueue(context.Background(), Task{Type: "sast.scan"}, Queue("sast")); err != nil {
		t.Fatalf("Enqueue() to a workload this process does not host error = %v", err)
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "task"}, Queue("undeclared")); err == nil {
		t.Fatal("Enqueue() to an undeclared queue error = nil")
	}
	if backend.calls.Load() != 1 {
		t.Fatalf("backend calls = %d, want only the accepted task", backend.calls.Load())
	}
}

// newWorkloadPlugin builds a plugin from explicit contributor entries, so a
// test can decide which workload each contributor belongs to.
func newWorkloadPlugin(t *testing.T, cfg Config, contributors ...plugin.Entry[HandlerContributor]) *Plugin {
	t.Helper()
	p, err := assemblePlugin(cfg, contributors, nil)
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	return p
}

func contributorEntry(key plugin.Key, workload plugin.WorkloadKey, taskTypes ...string) plugin.Entry[HandlerContributor] {
	registrations := make([]HandlerRegistration, 0, len(taskTypes))
	for _, taskType := range taskTypes {
		registrations = append(registrations, HandlerRegistration{
			Type:    taskType,
			Handler: HandlerFunc(func(context.Context, Task) error { return nil }),
		})
	}
	return plugin.Entry[HandlerContributor]{
		Identity: plugin.Identity{Plugin: key},
		Workload: workload,
		Value:    &testContributor{registrations: registrations},
	}
}

// eventLog orders observations from several goroutines: what the admission
// reported around a delivery and what the handler saw.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// recordingAdmission is a quota that notes every unit taken and given back.
type recordingAdmission struct {
	events *eventLog

	mu       sync.Mutex
	acquired int
	released int
}

type chargeCounts struct {
	acquired int
	released int
}

func (a *recordingAdmission) Acquire(context.Context) (func(), error) {
	a.mu.Lock()
	a.acquired++
	a.mu.Unlock()
	a.events.add("acquire")
	return func() {
		a.mu.Lock()
		a.released++
		a.mu.Unlock()
		a.events.add("release")
	}, nil
}

func (a *recordingAdmission) counts() chargeCounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	return chargeCounts{acquired: a.acquired, released: a.released}
}

type admissionFunc func(context.Context) (func(), error)

func (f admissionFunc) Acquire(ctx context.Context) (func(), error) { return f(ctx) }
