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

	"github.com/xbcio/xbc/extensions/tasks"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// The handles below are package-level because tasks.New and tasks.Method claim
// their names process-wide and panic on a duplicate definition, so defining
// them inside a test would explode the second time the package's tests run in
// one binary.

// testConsumed is the function task the consume tests configure this process
// to run. It validates its own argument so a test can tell a real execution
// from a binding that never ran.
var testConsumed = tasks.New("test.asynq.function", func(ctx context.Context, arg int) error {
	if arg != 7 {
		return errors.New("test.asynq.function received an argument other than 7")
	}
	return nil
})

// testRemotable is the method task the provider, submission, and dispatch
// tests share. Its receiver records every call and can fail on demand.
var testRemotable = tasks.Method("test.asynq.remotable", (*remoteService).handle)

// remoteService is the plugin value the shared method task hangs on.
type remoteService struct {
	calls chan string
	fail  error
}

func (s *remoteService) handle(ctx context.Context, arg string) error {
	s.calls <- arg
	return s.fail
}

// testProvider stands in for a plugin exporting tasks.Provider.
type testProvider struct {
	bindings []tasks.Binding
	panics   bool
}

func (p testProvider) Tasks() []tasks.Binding {
	if p.panics {
		panic("provider panic")
	}
	return p.bindings
}

// providerEntry wires a provider into the shape the definition collects.
func providerEntry(identity string, workload plugin.WorkloadKey, provider tasks.Provider) plugin.Entry[tasks.Provider] {
	return plugin.Entry[tasks.Provider]{
		Identity: plugin.Identity{Plugin: plugin.Key(identity)},
		Workload: workload,
		Value:    provider,
	}
}

// boundRemotable binds the shared method task to one receiver.
func boundRemotable(service *remoteService) []tasks.Binding {
	return tasks.Bind(service, testRemotable)
}

// receiveCall waits for one observed invocation of remoteService.handle.
func receiveCall(t *testing.T, service *remoteService) string {
	t.Helper()
	select {
	case observed := <-service.calls:
		return observed
	case <-time.After(2 * time.Second):
		t.Fatal("the task handler did not run")
		return ""
	}
}

// workerWorkloads names each worker group's workload, for failure messages.
func workerWorkloads(groups []*workerGroup) []plugin.WorkloadKey {
	workloads := make([]plugin.WorkloadKey, 0, len(groups))
	for _, group := range groups {
		workloads = append(workloads, group.workload)
	}
	return workloads
}

// dispatchGroup assembles one group from a single task binding, the way
// assemblePlugin does, so the dispatch tests can drive the real wrapper chain
// without Redis.
func dispatchGroup(t *testing.T, service *remoteService) *dispatcher {
	t.Helper()
	groups, err := groupHandlers(nil, []taskBinding{{
		binding: boundRemotable(service)[0],
		owner:   plugin.Identity{Plugin: "test-service"},
	}})
	if err != nil {
		t.Fatalf("groupHandlers() error = %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want one", workloadsOf(groups))
	}
	return groups[0].dispatcher
}

// assembleRemotePlugin is the common setup of the tests that drive the
// process-wide executor slot: a plugin whose unowned worker consumes the
// shared method task from its configured queue, over a miniredis instance.
func assembleRemotePlugin(t *testing.T, redisServer *miniredis.Miniredis, service *remoteService, taskConfig map[string]TaskConfig) *Plugin {
	t.Helper()
	cfg := validTestConfig(redisServer.Addr())
	cfg.Tasks = taskConfig
	p, err := assemblePlugin(cfg, nil, []plugin.Entry[tasks.Provider]{
		providerEntry("mail-service", "", testProvider{bindings: boundRemotable(service)}),
	})
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	return p
}

func TestAssemblePluginGroupsTaskBindingsByContributorWorkload(t *testing.T) {
	service := &remoteService{calls: make(chan string, 4)}
	cfg := defaultConfig()
	cfg.Workloads = map[string]WorkloadConfig{"mail": {Queues: map[string]int{"mail": 1}}}
	cfg.Tasks = map[string]TaskConfig{"test.asynq.remotable": {Queue: "mail"}}
	p, err := assemblePlugin(cfg, nil, []plugin.Entry[tasks.Provider]{
		providerEntry("mail-service", "mail", testProvider{bindings: boundRemotable(service)}),
	})
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	if len(p.groups) != 1 || p.groups[0].workload != "mail" {
		t.Fatalf("groups = %v, want one group for workload mail", workerWorkloads(p.groups))
	}
	if !reflect.DeepEqual(p.groups[0].queues, map[string]int{"mail": 1}) {
		t.Fatalf("mail worker queues = %v", p.groups[0].queues)
	}

	// The workload's worker dispatches the provider's task, decoding the
	// payload through the binding.
	if err := p.groups[0].dispatcher.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.remotable", []byte(`"hello"`))); err != nil {
		t.Fatalf("ProcessTask() error = %v", err)
	}
	if got := receiveCall(t, service); got != "hello" {
		t.Fatalf("handler observed %q", got)
	}

	// The same binding is what Run resolves through the executor's lookup.
	if _, ok := p.taskBindings["test.asynq.remotable"]; !ok {
		t.Fatalf("taskBindings = %v, want the provider's binding", p.taskBindings)
	}
}

func TestConsumeTrueAddsTheFunctionTaskToTheUnownedWorker(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"test.asynq.function": {Consume: true}}
	p, err := assemblePlugin(cfg, nil, nil)
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	if len(p.groups) != 1 || p.groups[0].workload != "" {
		t.Fatalf("groups = %v, want one unowned group", workerWorkloads(p.groups))
	}
	dispatch := p.groups[0].dispatcher
	if err := dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.function", []byte("7"))); err != nil {
		t.Fatalf("ProcessTask(7) error = %v", err)
	}
	if err := dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.function", []byte("8"))); err == nil {
		t.Fatal("ProcessTask(8) error = nil, want the function task's own validation to fail")
	}

	// Without consume the task is enqueueable but no worker exists for it: an
	// import that only references the definition must not start consuming it.
	cfg.Tasks = map[string]TaskConfig{"test.asynq.function": {}}
	p, err = assemblePlugin(cfg, nil, nil)
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	if len(p.groups) != 0 {
		t.Fatalf("groups = %v, want none for a task this process only enqueues", workerWorkloads(p.groups))
	}
	if len(p.taskBindings) != 0 {
		t.Fatalf("taskBindings = %v, want none", p.taskBindings)
	}
}

func TestConsumeTrueNeedsAFunctionTaskDefinedInThisProcess(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"test.asynq.missing": {Consume: true}}
	_, err := assemblePlugin(cfg, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"test.asynq.missing"`) || !strings.Contains(err.Error(), "consume: true") {
		t.Fatalf("assemblePlugin(undefined name) error = %v, want the name and consume: true named", err)
	}

	// A method task is consumed by selecting the plugin that provides it, so
	// consume: true on its name is refused too, before any grouping runs.
	service := &remoteService{calls: make(chan string, 1)}
	cfg.Tasks = map[string]TaskConfig{"test.asynq.remotable": {Consume: true}}
	_, err = assemblePlugin(cfg, nil, []plugin.Entry[tasks.Provider]{
		providerEntry("mail-service", "", testProvider{bindings: boundRemotable(service)}),
	})
	if err == nil || !strings.Contains(err.Error(), `"test.asynq.remotable"`) || !strings.Contains(err.Error(), "no tasks.New task") {
		t.Fatalf("assemblePlugin(method task) error = %v, want the method-consumption explanation", err)
	}
}

func TestTaskBindingsCollideWithAsynqHandlersInOneGroup(t *testing.T) {
	service := &remoteService{calls: make(chan string, 1)}
	_, err := assemblePlugin(defaultConfig(),
		[]plugin.Entry[HandlerContributor]{contributorEntry("business", "", "test.asynq.remotable")},
		[]plugin.Entry[tasks.Provider]{providerEntry("mail-service", "", testProvider{bindings: boundRemotable(service)})},
	)
	if err == nil {
		t.Fatal("assemblePlugin() error = nil")
	}
	for _, want := range []string{`"test.asynq.remotable"`, "business", "mail-service"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("assemblePlugin() error = %v, want %q named", err, want)
		}
	}

	// A consumed function task collides with an asynq handler of the same type
	// in the unowned group just the same.
	cfg := defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"test.asynq.function": {Consume: true}}
	_, err = assemblePlugin(cfg, []plugin.Entry[HandlerContributor]{contributorEntry("business", "", "test.asynq.function")}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate handler") || !strings.Contains(err.Error(), `"test.asynq.function"`) {
		t.Fatalf("assemblePlugin(consumed collision) error = %v", err)
	}
}

func TestConsumedTaskQueueMustBeFetchedByItsWorkerGroup(t *testing.T) {
	service := &remoteService{calls: make(chan string, 1)}
	provider := []plugin.Entry[tasks.Provider]{
		providerEntry("mail-service", "mail", testProvider{bindings: boundRemotable(service)}),
	}
	cfg := defaultConfig()
	cfg.Workloads = map[string]WorkloadConfig{"mail": {Queues: map[string]int{"mail": 1}}}

	// Without a tasks entry the task resolves to default_queue, which the mail
	// worker does not fetch: submissions would sit in Redis unconsumed here.
	_, err := assemblePlugin(cfg, nil, provider)
	if err == nil {
		t.Fatal("assemblePlugin() error = nil")
	}
	for _, want := range []string{`"test.asynq.remotable"`, `workload "mail"`, `"default"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("assemblePlugin() error = %v, want %q named", err, want)
		}
	}

	// Pointing the task at a queue its worker fetches satisfies the check.
	cfg.Tasks = map[string]TaskConfig{"test.asynq.remotable": {Queue: "mail"}}
	if _, err := assemblePlugin(cfg, nil, provider); err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}

	// A queue no configuration declares is refused wherever it is named.
	cfg.Tasks = map[string]TaskConfig{"test.asynq.remotable": {Queue: "nowhere"}}
	if _, err := prepareConfig(cfg); err == nil || !strings.Contains(err.Error(), `queue "nowhere" is not present in queues`) {
		t.Fatalf("prepareConfig() error = %v, want the unknown queue named", err)
	}

	// A process that only enqueues is not group-checked: naming a queue it does
	// not host is exactly the composition the split exists for.
	enqueuer := defaultConfig()
	enqueuer.Workloads = map[string]WorkloadConfig{"sast": {Queues: map[string]int{"sast": 1}}}
	enqueuer.Tasks = map[string]TaskConfig{"test.asynq.remotable": {Queue: "sast"}}
	if _, err := assemblePlugin(enqueuer, nil, nil); err != nil {
		t.Fatalf("assemblePlugin(enqueue-only) error = %v", err)
	}
}

func TestPrepareConfigValidatesTaskEntries(t *testing.T) {
	negative := -1
	zeroRetries := 0
	for name, test := range map[string]struct {
		task TaskConfig
		want string
	}{
		"unknown queue":    {task: TaskConfig{Queue: "nowhere"}, want: `queue "nowhere" is not present in queues`},
		"queue whitespace": {task: TaskConfig{Queue: " default"}, want: "must have no surrounding whitespace"},
		"negative retries": {task: TaskConfig{MaxRetries: &negative}, want: "max_retries cannot be negative"},
		"negative timeout": {task: TaskConfig{Timeout: -time.Second}, want: "timeout cannot be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Tasks = map[string]TaskConfig{"some.task": test.task}
			if _, err := prepareConfig(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prepareConfig() error = %v, want %q", err, test.want)
			}
		})
	}

	// Zero retries is a value, not a defaulted or invalid one.
	cfg := defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"some.task": {MaxRetries: &zeroRetries}}
	if _, err := prepareConfig(cfg); err != nil {
		t.Fatalf("prepareConfig(zero retries) error = %v", err)
	}

	// An empty task name cannot address anything.
	cfg = defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"": {}}
	if _, err := prepareConfig(cfg); err == nil || !strings.Contains(err.Error(), "task name") {
		t.Fatalf("prepareConfig(empty name) error = %v", err)
	}
}

func TestConfigCloneDeepCopiesTaskEntries(t *testing.T) {
	zero := 0
	cfg := defaultConfig()
	cfg.Tasks = map[string]TaskConfig{"some.task": {Queue: "default", MaxRetries: &zero}}

	cloned := cfg.clone()
	task := cloned.Tasks["some.task"]
	*task.MaxRetries = 9
	task.Queue = "changed"
	cloned.Tasks["some.task"] = task

	if *cfg.Tasks["some.task"].MaxRetries != 0 || cfg.Tasks["some.task"].Queue != "default" {
		t.Fatalf("clone aliased the original task entry: %+v", cfg.Tasks["some.task"])
	}
}

func TestRemoteSubmissionsCarryTheConfiguredQueueRetriesAndTimeout(t *testing.T) {
	redisServer := miniredis.RunT(t)
	service := &remoteService{calls: make(chan string, 4)}
	zeroRetries := 0
	p := assembleRemotePlugin(t, redisServer, service, map[string]TaskConfig{
		"test.asynq.remotable": {Queue: "default", MaxRetries: &zeroRetries, Timeout: 5 * time.Second},
	})
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })

	if err := testRemotable.Submit(context.Background(), "hello"); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	inspector := hibiken.NewInspectorFromRedisClient(p.redis)
	pending, err := inspector.ListPendingTasks("default")
	if err != nil {
		t.Fatalf("ListPendingTasks() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending tasks = %d, want 1", len(pending))
	}
	stored := pending[0]
	if stored.Type != "test.asynq.remotable" || stored.Queue != "default" {
		t.Fatalf("stored task = %+v", stored)
	}
	if stored.MaxRetry != 0 || stored.Timeout != 5*time.Second {
		t.Fatalf("stored task options = maxRetry %d timeout %s, want the configured 0 and 5s", stored.MaxRetry, stored.Timeout)
	}
	if string(stored.Payload) != `"hello"` {
		t.Fatalf("stored payload = %q", stored.Payload)
	}

	// Draining keeps the executor installed and the client usable: a submission
	// accepted after the drain still reaches Redis.
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if err := testRemotable.Submit(context.Background(), "after-drain"); err != nil {
		t.Fatalf("Submit() after drain error = %v", err)
	}
	if pending, err = inspector.ListPendingTasks("default"); err != nil || len(pending) != 2 {
		t.Fatalf("pending tasks after drain = %d (%v), want 2", len(pending), err)
	}

	// Stop uninstalls the executor once its client stopped admitting
	// submissions.
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if err := testRemotable.Submit(context.Background(), "too-late"); !errors.Is(err, tasks.ErrNotInstalled) {
		t.Fatalf("Submit() after stop error = %v, want ErrNotInstalled", err)
	}
}

func TestMethodRunResolvesThroughTheRemoteExecutor(t *testing.T) {
	// Before any executor is installed, Run reports no handler.
	if err := testRemotable.Run(context.Background(), "early"); !errors.Is(err, tasks.ErrNoHandler) {
		t.Fatalf("Run() before init error = %v, want ErrNoHandler", err)
	}

	redisServer := miniredis.RunT(t)
	service := &remoteService{calls: make(chan string, 4)}
	p := assembleRemotePlugin(t, redisServer, service, nil)
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	// The stop below asserts post-stop behavior; this guard keeps the
	// process-wide slot released even when an earlier assertion fails.
	t.Cleanup(func() { _ = p.stop(context.Background()) })

	// Run executes the providing plugin's method in this goroutine, without
	// going through Redis, so a process that only consumes can still run its
	// method tasks synchronously.
	if err := testRemotable.Run(context.Background(), "direct"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := receiveCall(t, service); got != "direct" {
		t.Fatalf("handler observed %q", got)
	}
	inspector := hibiken.NewInspectorFromRedisClient(p.redis)
	if pending, err := inspector.ListPendingTasks("default"); err != nil && !errors.Is(err, hibiken.ErrQueueNotFound) {
		t.Fatalf("ListPendingTasks() error = %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("pending tasks after Run = %d, want none", len(pending))
	}

	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if err := testRemotable.Run(context.Background(), "late"); !errors.Is(err, tasks.ErrNoHandler) {
		t.Fatalf("Run() after stop error = %v, want ErrNoHandler", err)
	}
	if err := testRemotable.Submit(context.Background(), "too-late"); !errors.Is(err, tasks.ErrNotInstalled) {
		t.Fatalf("Submit() after stop error = %v, want ErrNotInstalled", err)
	}
}

func TestDispatchMapsPermanentAndDecodeFailuresToSkipRetry(t *testing.T) {
	permanentCause := errors.New("this failure is final")
	permanent := dispatchGroup(t, &remoteService{calls: make(chan string, 1), fail: tasks.Permanent(permanentCause)})
	err := permanent.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.remotable", []byte(`"x"`)))
	if !errors.Is(err, hibiken.SkipRetry) || !errors.Is(err, tasks.ErrPermanent) || !errors.Is(err, permanentCause) {
		t.Fatalf("ProcessTask(permanent) error = %v, want SkipRetry with the marked error reachable", err)
	}

	// A payload the binding cannot decode is just as final: redelivery would
	// decode it to the same failure.
	err = permanent.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.remotable", []byte("{not json")))
	if !errors.Is(err, hibiken.SkipRetry) || !errors.Is(err, tasks.ErrPayload) {
		t.Fatalf("ProcessTask(undecodable) error = %v, want SkipRetry with ErrPayload reachable", err)
	}

	// Anything else follows the task's configured retry policy.
	retryable := errors.New("retry me")
	dispatch := dispatchGroup(t, &remoteService{calls: make(chan string, 1), fail: retryable})
	err = dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.remotable", []byte(`"x"`)))
	if !errors.Is(err, retryable) || errors.Is(err, hibiken.SkipRetry) {
		t.Fatalf("ProcessTask(retryable) error = %v, want the error as the handler returned it", err)
	}

	// A type with no handler retries and archives by the task's own config.
	err = dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.missing", nil))
	if !errors.Is(err, ErrHandlerNotFound) || errors.Is(err, hibiken.SkipRetry) {
		t.Fatalf("ProcessTask(unknown type) error = %v, want ErrHandlerNotFound without SkipRetry", err)
	}
}

func TestAssemblePluginRejectsInvalidAndPanickingProviders(t *testing.T) {
	_, err := assemblePlugin(defaultConfig(), nil, []plugin.Entry[tasks.Provider]{
		providerEntry("bad", "", testProvider{bindings: []tasks.Binding{{}}}),
	})
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "invalid task name") {
		t.Fatalf("assemblePlugin(zero binding) error = %v", err)
	}

	_, err = assemblePlugin(defaultConfig(), nil, []plugin.Entry[tasks.Provider]{
		providerEntry("panicking", "", testProvider{panics: true}),
	})
	if err == nil || !strings.Contains(err.Error(), "panicking") || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("assemblePlugin(panicking provider) error = %v", err)
	}
}

func TestInstallingASecondRemoteExecutorKeepsTheFirstBinding(t *testing.T) {
	backend := &countingEnqueueBackend{}
	uninstallFirst := installRemoteExecutor(&remoteExecutor{client: newClient(backend, defaultConfig())}, nil)
	defer uninstallFirst()

	// The install took the slot: a submission now reaches the first executor's
	// backend. If an earlier test had leaked the slot, this install would have
	// been the no-op one and the submission would not arrive here.
	if err := testRemotable.Submit(context.Background(), "first"); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if calls := backend.calls.Load(); calls != 1 {
		t.Fatalf("submissions reaching the first executor = %d, want 1", calls)
	}

	logger := &captureLogger{}
	uninstallSecond := installRemoteExecutor(&remoteExecutor{}, logger)
	if !logger.recorded("asynq: a remote task executor is already installed; keeping the existing binding") {
		t.Fatal("the second executor did not warn; it must keep the existing binding instead of taking the slot")
	}

	uninstallSecond()
	if err := testRemotable.Submit(context.Background(), "still-first"); err != nil {
		t.Fatalf("Submit() after the second uninstall error = %v", err)
	}
	if calls := backend.calls.Load(); calls != 2 {
		t.Fatalf("submissions reaching the first executor = %d, want 2", calls)
	}
}

func TestRemoteExecutorReportsAClosedClientAsTasksErrClosed(t *testing.T) {
	client := newClient(&countingEnqueueBackend{}, defaultConfig())
	executor := &remoteExecutor{client: client}
	client.beginClose()

	err := executor.Submit(context.Background(), "test.asynq.remotable", []byte(`"late"`))
	if !errors.Is(err, tasks.ErrClosed) || !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit() on a closed client error = %v, want both sentinels reachable", err)
	}
}

// captureLogger records the messages a test asserts on, satisfying the parts
// of the log.Logger interface this package's tests need.
type captureLogger struct {
	mu       sync.Mutex
	messages []string
}

func (l *captureLogger) add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, msg)
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.add(msg) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add(msg) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add(msg) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add(msg) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) corelog.Logger  { return l }
func (*captureLogger) Enabled(corelog.Level) bool    { return true }

func (l *captureLogger) recorded(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, recorded := range l.messages {
		if recorded == msg {
			return true
		}
	}
	return false
}

// localStub is a local executor that holds no bindings, standing in for an
// async pool that installed itself and has since been stopped.
type localStub struct{}

func (localStub) Go(context.Context, string, func(context.Context)) error { return nil }
func (localStub) Submit(context.Context, string, []byte) error            { return nil }
func (localStub) Lookup(string) (tasks.Binding, bool)                     { return tasks.Binding{}, false }

// TestDispatchOutlivesTheLocalExecutor pins that a delivered method task
// reaches its handler through asynq's own dispatch table: async stops before
// asynq under the default shutdown sequence, and a task asynq's worker
// dequeues after that must still run rather than fail with no handler.
func TestDispatchOutlivesTheLocalExecutor(t *testing.T) {
	uninstall, installed := tasks.InstallLocal(localStub{})
	if !installed {
		t.Fatal("InstallLocal() did not install the stub; a previous test left the slot taken")
	}
	uninstall()
	if err := tasks.Go(context.Background(), func(context.Context) {}); !errors.Is(err, tasks.ErrNotInstalled) {
		t.Fatalf("Go() after uninstall error = %v, want ErrNotInstalled", err)
	}

	service := &remoteService{calls: make(chan string, 1)}
	dispatch := dispatchGroup(t, service)
	if err := dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.remotable", []byte(`"after-async"`))); err != nil {
		t.Fatalf("ProcessTask() after the local executor left error = %v", err)
	}
	if got := receiveCall(t, service); got != "after-async" {
		t.Fatalf("handler observed %q", got)
	}
}
