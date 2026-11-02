package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

func TestControllerMovesArchivesDeletesAndPauses(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)

	enqueueInspectionTask(t, p, "work", "pending-1")
	enqueueInspectionTask(t, p, "work", "scheduled-1", ProcessIn(time.Hour))

	if err := p.RunTask("default", "scheduled-1"); err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	task, err := p.GetTaskInfo("default", "scheduled-1")
	if err != nil {
		t.Fatalf("GetTaskInfo() error = %v", err)
	}
	if task.State != StatePending || task.ID != "scheduled-1" || task.Type != "work" {
		t.Fatalf("task after RunTask() = %+v", task)
	}
	requireIDs(t, listedIDs(t, p, "default", StateScheduled))
	requireIDs(t, listedIDs(t, p, "default", StatePending), "pending-1", "scheduled-1")

	if err := p.ArchiveTask("default", "scheduled-1"); err != nil {
		t.Fatalf("ArchiveTask() error = %v", err)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "scheduled-1")

	if err := p.DeleteTask("default", "scheduled-1"); err != nil {
		t.Fatalf("DeleteTask() error = %v", err)
	}
	if _, err := p.GetTaskInfo("default", "scheduled-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("GetTaskInfo(deleted) error = %v, want ErrTaskNotFound", err)
	}
	if err := p.DeleteTask("default", "scheduled-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("second DeleteTask() error = %v, want ErrTaskNotFound", err)
	}

	enqueueInspectionTask(t, p, "work", "archived-1")
	enqueueInspectionTask(t, p, "work", "archived-2")
	for _, id := range []string{"archived-1", "archived-2"} {
		if err := p.ArchiveTask("default", id); err != nil {
			t.Fatalf("ArchiveTask(%q) error = %v", id, err)
		}
	}
	deleted, err := p.DeleteAllByState("default", StateArchived)
	if err != nil {
		t.Fatalf("DeleteAllByState(archived) error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteAllByState(archived) deleted %d, want 2", deleted)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived))

	if deleted, err := p.DeleteAllByState("default", StateCompleted); err != nil || deleted != 0 {
		t.Fatalf("DeleteAllByState(completed) = %d, %v; want 0, nil", deleted, err)
	}
	if _, err := p.DeleteAllByState("default", StateActive); !errors.Is(err, ErrUnsupportedState) {
		t.Fatalf("DeleteAllByState(active) error = %v, want ErrUnsupportedState", err)
	}
	if _, err := p.DeleteAllByState("default", StateUnknown); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("DeleteAllByState(unknown) error = %v, want ErrInvalidState", err)
	}
	if _, err := p.DeleteAllByState("default", State(99)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("DeleteAllByState(invalid) error = %v, want ErrInvalidState", err)
	}

	if err := p.PauseQueue("default"); err != nil {
		t.Fatalf("PauseQueue() error = %v", err)
	}
	info, err := p.QueueInfo("default")
	if err != nil {
		t.Fatalf("QueueInfo() error = %v", err)
	}
	if !info.Paused {
		t.Fatal("QueueInfo() reports an unpaused queue after PauseQueue")
	}
	if err := p.PauseQueue("default"); err == nil {
		t.Fatal("second PauseQueue() error = nil")
	}
	if err := p.UnpauseQueue("default"); err != nil {
		t.Fatalf("UnpauseQueue() error = %v", err)
	}
	info, err = p.QueueInfo("default")
	if err != nil {
		t.Fatalf("QueueInfo() after unpause error = %v", err)
	}
	if info.Paused {
		t.Fatal("QueueInfo() reports a paused queue after UnpauseQueue")
	}
	if err := p.UnpauseQueue("default"); err == nil {
		t.Fatal("second UnpauseQueue() error = nil")
	}
}

func TestInspectorAndControllerRejectUndeclaredQueues(t *testing.T) {
	p, redisServer, _ := initInspectionPlugin(t, nil, nil)
	foreignClient, foreignInspector := newForeignBackend(t, redisServer)

	if _, err := foreignClient.Enqueue(hibiken.NewTask("foreign-work", nil), hibiken.Queue("foreign"), hibiken.TaskID("foreign-pending")); err != nil {
		t.Fatalf("foreign Enqueue() error = %v", err)
	}
	if _, err := foreignClient.Enqueue(hibiken.NewTask("foreign-work", nil), hibiken.Queue("foreign"), hibiken.TaskID("foreign-archived")); err != nil {
		t.Fatalf("foreign Enqueue() error = %v", err)
	}
	if err := foreignInspector.ArchiveTask("foreign", "foreign-archived"); err != nil {
		t.Fatalf("foreign ArchiveTask() error = %v", err)
	}

	assertNotOwned := func(operation string, err error) {
		t.Helper()
		if !errors.Is(err, ErrQueueNotOwned) {
			t.Fatalf("%s error = %v, want ErrQueueNotOwned", operation, err)
		}
	}

	_, err := p.QueueInfo("foreign")
	assertNotOwned("QueueInfo", err)
	_, err = p.ListTasks("foreign", StatePending, Page{})
	assertNotOwned("ListTasks", err)
	_, err = p.GetTaskInfo("foreign", "foreign-pending")
	assertNotOwned("GetTaskInfo", err)
	assertNotOwned("DeleteTask", p.DeleteTask("foreign", "foreign-pending"))
	_, err = p.DeleteAllByState("foreign", StatePending)
	assertNotOwned("DeleteAllByState", err)
	assertNotOwned("RunTask", p.RunTask("foreign", "foreign-pending"))
	assertNotOwned("ArchiveTask", p.ArchiveTask("foreign", "foreign-pending"))
	assertNotOwned("PauseQueue", p.PauseQueue("foreign"))
	assertNotOwned("UnpauseQueue", p.UnpauseQueue("foreign"))
	_, err = p.PurgeArchived("foreign", time.Time{})
	assertNotOwned("PurgeArchived", err)
	if err := p.CancelProcessing("foreign-pending"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelProcessing(foreign task) error = %v, want ErrTaskNotFound", err)
	}

	queues, err := p.Queues()
	if err != nil {
		t.Fatalf("Queues() error = %v", err)
	}
	requireStrings(t, queues, "default")

	// The rejected calls must not have touched the foreign queue.
	info, err := foreignInspector.GetQueueInfo("foreign")
	if err != nil {
		t.Fatalf("foreign GetQueueInfo() error = %v", err)
	}
	if info.Pending != 1 || info.Archived != 1 || info.Paused {
		t.Fatalf("foreign queue changed after rejected operations: %+v", info)
	}
	task, err := foreignInspector.GetTaskInfo("foreign", "foreign-pending")
	if err != nil {
		t.Fatalf("foreign task is gone: %v", err)
	}
	if task.State != hibiken.TaskStatePending || task.Type != "foreign-work" {
		t.Fatalf("foreign task changed: %+v", task)
	}
	archived, err := foreignInspector.GetTaskInfo("foreign", "foreign-archived")
	if err != nil {
		t.Fatalf("foreign archived task is gone: %v", err)
	}
	if archived.State != hibiken.TaskStateArchived {
		t.Fatalf("foreign archived task state = %v", archived.State)
	}
}

func TestPurgeArchivedRemovesOnlyTasksOlderThanCutoff(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, func(cfg *Config) {
		cfg.Queues = map[string]int{"default": 1, "idle": 1}
	})
	enqueueInspectionTask(t, p, "work", "old-1")
	enqueueInspectionTask(t, p, "work", "new-1")
	for _, id := range []string{"old-1", "new-1"} {
		if err := p.ArchiveTask("default", id); err != nil {
			t.Fatalf("ArchiveTask(%q) error = %v", id, err)
		}
	}

	redisClient, _, err := p.backends()
	if err != nil {
		t.Fatalf("backends() error = %v", err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if err := redisClient.ZAdd(context.Background(), queueKey("default", "archived"), goredis.Z{
		Score:  float64(old),
		Member: "old-1",
	}).Err(); err != nil {
		t.Fatalf("age the archived task: %v", err)
	}

	purged, err := p.PurgeArchived("default", time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeArchived(cutoff) error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeArchived(cutoff) purged %d tasks, want 1", purged)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "new-1")

	purged, err = p.PurgeArchived("default", time.Time{})
	if err != nil {
		t.Fatalf("PurgeArchived(all) error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeArchived(all) purged %d tasks, want 1", purged)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived))

	purged, err = p.PurgeArchived("default", time.Now())
	if err != nil || purged != 0 {
		t.Fatalf("PurgeArchived(empty) = %d, %v; want 0, nil", purged, err)
	}
	if _, err := p.PurgeArchived("idle", time.Time{}); !errors.Is(err, ErrQueueNotFound) {
		t.Fatalf("PurgeArchived(unused queue) error = %v, want ErrQueueNotFound", err)
	}
}
