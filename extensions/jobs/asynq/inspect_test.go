package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

// initInspectionPlugin builds and initializes a plugin against a fresh
// miniredis. It does not start a worker; tests that need one start it
// themselves with the returned host. A nil handler registers a no-op handler.
func initInspectionPlugin(t *testing.T, handler Handler, customize func(*Config)) (*Plugin, *miniredis.Miniredis, *testHost) {
	t.Helper()
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	if customize != nil {
		customize(&cfg)
	}
	if handler == nil {
		handler = HandlerFunc(func(context.Context, Task) error { return nil })
	}
	p := newTestPlugin(t, cfg, handler)
	host := newTestHost()
	t.Cleanup(host.stopTasks)
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	return p, redisServer, host
}

// initInspectionPluginForTypes builds a plugin whose single contributor
// registers handler for each of types, so a worker test can exercise several
// task types at once.
func initInspectionPluginForTypes(t *testing.T, types []string, handler Handler) (*Plugin, *testHost) {
	t.Helper()
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	registrations := make([]HandlerRegistration, 0, len(types))
	for _, taskType := range types {
		registrations = append(registrations, HandlerRegistration{Type: taskType, Handler: handler})
	}
	p, err := assemblePlugin(cfg, []plugin.Entry[HandlerContributor]{{
		Identity: plugin.Identity{Plugin: "business"},
		Value:    &testContributor{registrations: registrations},
	}}, nil)
	if err != nil {
		t.Fatalf("assemblePlugin() error = %v", err)
	}
	host := newTestHost()
	t.Cleanup(host.stopTasks)
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	return p, host
}

func enqueueInspectionTask(t *testing.T, p *Plugin, taskType, id string, options ...TaskOption) TaskInfo {
	t.Helper()
	info, err := p.Enqueue(context.Background(), Task{Type: taskType, Payload: []byte(id)}, append([]TaskOption{TaskID(id), Queue("default")}, options...)...)
	if err != nil {
		t.Fatalf("Enqueue(%q) error = %v", id, err)
	}
	return info
}

func listedIDs(t *testing.T, p *Plugin, queue string, state State) []string {
	t.Helper()
	tasks, err := p.ListTasks(queue, state, Page{Size: 100})
	if err != nil {
		t.Fatalf("ListTasks(%q, %s) error = %v", queue, state, err)
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func requireIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("task IDs = %v, want %v", got, want)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("task IDs = %v, want %v", got, want)
		}
	}
}

func TestInspectorReportsQueuesCountsAndPages(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, func(cfg *Config) {
		cfg.Queues = map[string]int{"default": 1, "idle": 1}
		cfg.Workloads = map[string]WorkloadConfig{
			"scan": {Queues: map[string]int{"scan-q": 1}},
		}
	})

	enqueueInspectionTask(t, p, "work", "pending-1")
	enqueueInspectionTask(t, p, "work", "pending-2")
	enqueueInspectionTask(t, p, "work", "pending-3")
	enqueueInspectionTask(t, p, "work", "scheduled-1", ProcessIn(time.Hour))
	if err := p.ArchiveTask("default", "pending-3"); err != nil {
		t.Fatalf("ArchiveTask() error = %v", err)
	}

	queues, err := p.Queues()
	if err != nil {
		t.Fatalf("Queues() error = %v", err)
	}
	requireStrings(t, queues, "default", "idle", "scan-q")

	info, err := p.QueueInfo("default")
	if err != nil {
		t.Fatalf("QueueInfo() error = %v", err)
	}
	if info.Queue != "default" || info.Pending != 2 || info.Active != 0 || info.Scheduled != 1 ||
		info.Retry != 0 || info.Archived != 1 || info.Completed != 0 || info.Size != 4 || info.Paused {
		t.Fatalf("QueueInfo() = %+v", info)
	}

	first, err := p.ListTasks("default", StatePending, Page{Number: 1, Size: 1})
	if err != nil {
		t.Fatalf("ListTasks(page 1) error = %v", err)
	}
	second, err := p.ListTasks("default", StatePending, Page{Number: 2, Size: 1})
	if err != nil {
		t.Fatalf("ListTasks(page 2) error = %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("page sizes = %d/%d, want 1/1", len(first), len(second))
	}
	requireIDs(t, []string{first[0].ID, second[0].ID}, "pending-1", "pending-2")

	requireIDs(t, listedIDs(t, p, "default", StateScheduled), "scheduled-1")
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "pending-3")

	if _, err := p.ListTasks("default", State(99), Page{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ListTasks(invalid state) error = %v, want ErrInvalidState", err)
	}
	if _, err := p.ListTasks("foreign", StatePending, Page{}); !errors.Is(err, ErrQueueNotOwned) {
		t.Fatalf("ListTasks(foreign queue) error = %v, want ErrQueueNotOwned", err)
	}

	task, err := p.GetTaskInfo("default", "pending-1")
	if err != nil {
		t.Fatalf("GetTaskInfo() error = %v", err)
	}
	if task.ID != "pending-1" || task.Queue != "default" || task.Type != "work" || task.State != StatePending ||
		string(task.Payload) != "pending-1" || task.MaxRetries != p.cfg.DefaultMaxRetries {
		t.Fatalf("GetTaskInfo() = %+v", task)
	}
	if _, err := p.GetTaskInfo("default", "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("GetTaskInfo(missing) error = %v, want ErrTaskNotFound", err)
	}
	if _, err := p.GetTaskInfo("idle", "pending-1"); !errors.Is(err, ErrQueueNotFound) {
		t.Fatalf("GetTaskInfo(unused queue) error = %v, want ErrQueueNotFound", err)
	}
	if _, err := p.GetTaskInfo("foreign", "pending-1"); !errors.Is(err, ErrQueueNotOwned) {
		t.Fatalf("GetTaskInfo(foreign queue) error = %v, want ErrQueueNotOwned", err)
	}
	if _, err := p.QueueInfo("idle"); !errors.Is(err, ErrQueueNotFound) {
		t.Fatalf("QueueInfo(unused queue) error = %v, want ErrQueueNotFound", err)
	}

	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if _, err := p.QueueInfo("default"); !errors.Is(err, ErrClosed) {
		t.Fatalf("QueueInfo() after stop error = %v, want ErrClosed", err)
	}
	if _, err := p.Queues(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Queues() after stop error = %v, want ErrClosed", err)
	}
}

func TestInspectorReportsRetryCompletedAndActiveStates(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := HandlerFunc(func(ctx context.Context, task Task) error {
		switch task.Type {
		case "fail":
			return errors.New("boom")
		case "block":
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		default:
			return nil
		}
	})
	p, host := initInspectionPluginForTypes(t, []string{"work", "fail", "block"}, handler)
	if err := p.start(testContext(host)); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	host.openTraffic()

	enqueueInspectionTask(t, p, "fail", "retry-1", MaxRetries(2))
	enqueueInspectionTask(t, p, "work", "done-1", Retention(time.Hour))
	enqueueInspectionTask(t, p, "block", "active-1")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start the blocking task")
	}

	waitForCondition(t, 5*time.Second, func() bool {
		ids := listedIDs(t, p, "default", StateRetry)
		return len(ids) == 1 && ids[0] == "retry-1"
	}, "the failed task to enter retry")

	waitForCondition(t, 5*time.Second, func() bool {
		ids := listedIDs(t, p, "default", StateCompleted)
		return len(ids) == 1 && ids[0] == "done-1"
	}, "the successful task to enter completed")

	requireIDs(t, listedIDs(t, p, "default", StateActive), "active-1")
	active, err := p.ListTasks("default", StateActive, Page{})
	if err != nil {
		t.Fatalf("ListTasks(active) error = %v", err)
	}
	if active[0].State != StateActive {
		t.Fatalf("active task state = %s, want active", active[0].State)
	}
	if err := p.CancelProcessing("active-1"); err != nil {
		t.Fatalf("CancelProcessing() error = %v", err)
	}

	close(release)
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

func TestCancelProcessingReportsMissingTask(t *testing.T) {
	p, redisServer, _ := initInspectionPlugin(t, nil, nil)
	if err := p.CancelProcessing("no-such-task"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelProcessing(unknown) error = %v, want ErrTaskNotFound", err)
	}
	if err := p.CancelProcessing(""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelProcessing(empty) error = %v, want ErrTaskNotFound", err)
	}
	// A task that exists in a queue this plugin does not declare must not be
	// reachable through the queue-less cancel signal.
	enqueueForeignTask(t, redisServer, "foreign-cancel")
	if err := p.CancelProcessing("foreign-cancel"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelProcessing(foreign task) error = %v, want ErrTaskNotFound", err)
	}
}

// requireStrings asserts an exact string slice.
func requireStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("strings = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("strings = %v, want %v", got, want)
		}
	}
}

// enqueueForeignTask creates and archives one task in a queue the plugin does
// not declare, through an independent client on the same Redis instance.
func enqueueForeignTask(t *testing.T, redisServer *miniredis.Miniredis, id string) {
	t.Helper()
	client, inspector := newForeignBackend(t, redisServer)
	if _, err := client.Enqueue(hibiken.NewTask("foreign-work", nil), hibiken.Queue("foreign"), hibiken.TaskID(id)); err != nil {
		t.Fatalf("foreign Enqueue() error = %v", err)
	}
	if err := inspector.ArchiveTask("foreign", id); err != nil {
		t.Fatalf("foreign ArchiveTask() error = %v", err)
	}
}

// newForeignBackend returns a vendor client and inspector on the shared
// miniredis, independent of the plugin under test.
func newForeignBackend(t *testing.T, redisServer *miniredis.Miniredis) (*hibiken.Client, *hibiken.Inspector) {
	t.Helper()
	client := hibiken.NewClient(hibiken.RedisClientOpt{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	inspector := hibiken.NewInspector(hibiken.RedisClientOpt{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = inspector.Close() })
	return client, inspector
}
