package asynq

import (
	"context"
	"errors"
	"fmt"
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

// archiveInspectionTasks archives each already-enqueued task in the default
// queue, failing the test on the first error.
func archiveInspectionTasks(t *testing.T, p *Plugin, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := p.ArchiveTask("default", id); err != nil {
			t.Fatalf("ArchiveTask(%q) error = %v", id, err)
		}
	}
}

func TestPurgeArchivedFiltersByTaskType(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	enqueueInspectionTask(t, p, "alpha", "alpha-1")
	enqueueInspectionTask(t, p, "beta", "beta-1")
	archiveInspectionTasks(t, p, "alpha-1", "beta-1")

	purged, err := p.PurgeArchived("default", time.Time{}, "alpha")
	if err != nil {
		t.Fatalf("PurgeArchived(type) error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeArchived(type) purged %d tasks, want 1", purged)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "beta-1")

	purged, err = p.PurgeArchived("default", time.Time{}, "alpha")
	if err != nil || purged != 0 {
		t.Fatalf("second PurgeArchived(type) = %d, %v; want 0, nil", purged, err)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "beta-1")

	for _, taskType := range []string{"", "  ", " alpha"} {
		if _, err := p.PurgeArchived("default", time.Time{}, taskType); err == nil {
			t.Fatalf("PurgeArchived(type %q) error = nil, want an invalid task type error", taskType)
		}
	}
}

func TestPurgeArchivedCombinesCutoffAndTaskType(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	enqueueInspectionTask(t, p, "alpha", "old-alpha")
	enqueueInspectionTask(t, p, "alpha", "new-alpha")
	enqueueInspectionTask(t, p, "beta", "old-beta")
	archiveInspectionTasks(t, p, "old-alpha", "new-alpha", "old-beta")

	redisClient, _, err := p.backends()
	if err != nil {
		t.Fatalf("backends() error = %v", err)
	}
	old := float64(time.Now().Add(-100 * 24 * time.Hour).Unix())
	ctx := context.Background()
	for _, id := range []string{"old-alpha", "old-beta"} {
		if err := redisClient.ZAdd(ctx, queueKey("default", "archived"), goredis.Z{Score: old, Member: id}).Err(); err != nil {
			t.Fatalf("age archived task %q: %v", id, err)
		}
	}

	purged, err := p.PurgeArchived("default", time.Now().Add(-24*time.Hour), "alpha")
	if err != nil {
		t.Fatalf("PurgeArchived(cutoff, type) error = %v", err)
	}
	// old-beta is older than the cutoff but of another type, so it stays.
	if purged != 1 {
		t.Fatalf("PurgeArchived(cutoff, type) purged %d tasks, want 1", purged)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "new-alpha", "old-beta")
}

func TestPurgeArchivedAcceptsMultipleTaskTypes(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	enqueueInspectionTask(t, p, "alpha", "alpha-1")
	enqueueInspectionTask(t, p, "beta", "beta-1")
	enqueueInspectionTask(t, p, "gamma", "gamma-1")
	archiveInspectionTasks(t, p, "alpha-1", "beta-1", "gamma-1")

	purged, err := p.PurgeArchived("default", time.Time{}, "alpha", "beta")
	if err != nil {
		t.Fatalf("PurgeArchived(types) error = %v", err)
	}
	if purged != 2 {
		t.Fatalf("PurgeArchived(types) purged %d tasks, want 2", purged)
	}
	requireIDs(t, listedIDs(t, p, "default", StateArchived), "gamma-1")
}

func TestPurgeArchivedRemovesOrphansWithTypeFilter(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	// Register the queue with Redis without touching the archive.
	enqueueInspectionTask(t, p, "alpha", "pending-1")

	redisClient, _, err := p.backends()
	if err != nil {
		t.Fatalf("backends() error = %v", err)
	}
	ctx := context.Background()
	archivedKey := queueKey("default", "archived")
	if err := redisClient.ZAdd(ctx, archivedKey, goredis.Z{
		Score:  float64(time.Now().Unix()),
		Member: "orphan-1",
	}).Err(); err != nil {
		t.Fatalf("add orphan archive entry: %v", err)
	}

	purged, err := p.PurgeArchived("default", time.Time{}, "alpha")
	if err != nil {
		t.Fatalf("PurgeArchived(orphan, type) error = %v", err)
	}
	if purged != 0 {
		t.Fatalf("PurgeArchived(orphan, type) purged %d tasks, want 0", purged)
	}
	if count, err := redisClient.ZCard(ctx, archivedKey).Result(); err != nil || count != 0 {
		t.Fatalf("archived sorted set after orphan cleanup = %d, %v; want 0, nil", count, err)
	}
}

func TestPurgeArchivedPagesPastRetainedTasks(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	// One more non-matching archive entry than a purge batch forces a second
	// read, and the matching task is archived last so it sorts after all of
	// them. A fixed offset would never reach it.
	const kept = purgeBatchSize + 1
	ids := make([]string, 0, kept+1)
	for index := 0; index < kept; index++ {
		id := fmt.Sprintf("bulk-%04d", index)
		enqueueInspectionTask(t, p, "bulk", id)
		ids = append(ids, id)
	}
	enqueueInspectionTask(t, p, "target", "zzz-target")
	ids = append(ids, "zzz-target")
	archiveInspectionTasks(t, p, ids...)

	purged, err := p.PurgeArchived("default", time.Time{}, "target")
	if err != nil {
		t.Fatalf("PurgeArchived(paged, type) error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeArchived(paged, type) purged %d tasks, want 1", purged)
	}
	redisClient, _, err := p.backends()
	if err != nil {
		t.Fatalf("backends() error = %v", err)
	}
	ctx := context.Background()
	archivedKey := queueKey("default", "archived")
	if count, err := redisClient.ZCard(ctx, archivedKey).Result(); err != nil || count != kept {
		t.Fatalf("archived sorted set after paged purge = %d, %v; want %d, nil", count, err, kept)
	}
	if _, err := redisClient.ZScore(ctx, archivedKey, "zzz-target").Result(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("matching task remains archived: %v", err)
	}

	purged, err = p.PurgeArchived("default", time.Time{}, "target")
	if err != nil || purged != 0 {
		t.Fatalf("second PurgeArchived(paged, type) = %d, %v; want 0, nil", purged, err)
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
