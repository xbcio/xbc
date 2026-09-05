package asynq

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

func TestClientEnqueuePersistsNeutralTaskAndOptions(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := validTestConfig(server.Addr())
	cfg.Queues = map[string]int{"default": 1, "critical": 5}
	redisClient := goredis.NewClient(cfg.Redis.options())
	t.Cleanup(func() { _ = redisClient.Close() })
	client := newClient(hibiken.NewClientFromRedisClient(redisClient), cfg)
	inspector := hibiken.NewInspectorFromRedisClient(redisClient)

	payload := []byte("payload")
	headers := map[string]string{"trace": "abc"}
	deadline := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	info, err := client.Enqueue(context.Background(), Task{Type: "email.send", Payload: payload, Headers: headers},
		Queue("critical"), MaxRetries(4), Timeout(7*time.Second), Deadline(deadline),
		ProcessIn(5*time.Minute), TaskID("task-123"), Retention(2*time.Minute))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if info.ID != "task-123" || info.Queue != "critical" || info.Type != "email.send" || info.MaxRetries != 4 || info.Timeout != 7*time.Second {
		t.Fatalf("Enqueue() info = %+v", info)
	}
	if info.NextProcessAt.Before(time.Now().Add(4*time.Minute)) || info.NextProcessAt.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("NextProcessAt = %s, want approximately five minutes from now", info.NextProcessAt)
	}

	payload[0] = 'X'
	headers["trace"] = "changed"
	stored, err := inspector.GetTaskInfo("critical", info.ID)
	if err != nil {
		t.Fatalf("Inspector.GetTaskInfo() error = %v", err)
	}
	if string(stored.Payload) != "payload" || !reflect.DeepEqual(stored.Headers, map[string]string{"trace": "abc"}) {
		t.Fatalf("stored task payload/headers = %q/%v", stored.Payload, stored.Headers)
	}
	if stored.Deadline != deadline || stored.Retention != 2*time.Minute || stored.MaxRetry != 4 || stored.Timeout != 7*time.Second {
		t.Fatalf("stored task options = %+v", stored)
	}
}

func TestClientMapsUniqueAndTaskIDConflicts(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := validTestConfig(server.Addr())
	redisClient := goredis.NewClient(cfg.Redis.options())
	t.Cleanup(func() { _ = redisClient.Close() })
	client := newClient(hibiken.NewClientFromRedisClient(redisClient), cfg)
	ctx := context.Background()

	task := Task{Type: "deduplicated", Payload: []byte("same")}
	if _, err := client.Enqueue(ctx, task, UniqueFor(time.Minute)); err != nil {
		t.Fatalf("first unique Enqueue() error = %v", err)
	}
	if _, err := client.Enqueue(ctx, task, UniqueFor(time.Minute)); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	}
	if _, err := client.Enqueue(ctx, Task{Type: "first"}, TaskID("fixed-id")); err != nil {
		t.Fatalf("first TaskID Enqueue() error = %v", err)
	}
	if _, err := client.Enqueue(ctx, Task{Type: "second"}, TaskID("fixed-id")); !errors.Is(err, ErrTaskIDConflict) {
		t.Fatalf("conflicting TaskID Enqueue() error = %v", err)
	}
}

func TestClientRejectsInvalidTasksAndOptionsBeforeBackend(t *testing.T) {
	backend := &countingEnqueueBackend{}
	client := newClient(backend, defaultConfig())
	var nilOption TaskOption
	tests := []struct {
		name    string
		ctx     context.Context
		task    Task
		options []TaskOption
	}{
		{name: "nil context", task: Task{Type: "valid"}},
		{name: "empty type", ctx: context.Background(), task: Task{}},
		{name: "type whitespace", ctx: context.Background(), task: Task{Type: " valid"}},
		{name: "header whitespace", ctx: context.Background(), task: Task{Type: "valid", Headers: map[string]string{" trace": "x"}}},
		{name: "nil option", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{nilOption}},
		{name: "unknown queue", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{Queue("other")}},
		{name: "queue whitespace", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{Queue(" default")}},
		{name: "negative retries", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{MaxRetries(-1)}},
		{name: "zero timeout", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{Timeout(0)}},
		{name: "bad deadline", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{Deadline(time.Unix(0, 0))}},
		{name: "zero process time", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{ProcessAt(time.Time{})}},
		{name: "negative delay", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{ProcessIn(-time.Second)}},
		{name: "short unique", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{UniqueFor(time.Millisecond)}},
		{name: "task ID whitespace", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{TaskID(" id")}},
		{name: "negative retention", ctx: context.Background(), task: Task{Type: "valid"}, options: []TaskOption{Retention(-time.Second)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.Enqueue(test.ctx, test.task, test.options...); err == nil {
				t.Fatal("Enqueue() error = nil")
			}
		})
	}
	if backend.calls.Load() != 0 {
		t.Fatalf("backend calls = %d, want 0", backend.calls.Load())
	}
}

func TestClientOptionPrecedenceAndBackendErrors(t *testing.T) {
	processAt := time.Now().Add(time.Hour)
	backend := &captureEnqueueBackend{err: errors.New("transport down")}
	client := newClient(backend, defaultConfig())
	_, err := client.Enqueue(context.Background(), Task{Type: "scheduled"}, ProcessIn(time.Minute), ProcessAt(processAt))
	if err == nil || !errors.Is(err, backend.err) {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if backend.task == nil || backend.task.Type() != "scheduled" {
		t.Fatalf("backend task = %#v", backend.task)
	}
}

func TestClientCloseWaitsForAdmittedEnqueueAndRejectsLaterCalls(t *testing.T) {
	backend := &blockingEnqueueBackend{started: make(chan struct{}), release: make(chan struct{})}
	client := newClient(backend, defaultConfig())
	enqueueDone := make(chan error, 1)
	go func() {
		_, err := client.Enqueue(context.Background(), Task{Type: "running"})
		enqueueDone <- err
	}()
	<-backend.started
	closeDone := make(chan struct{})
	go func() {
		client.close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned while an admitted enqueue was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.release)
	if err := <-enqueueDone; err != nil {
		t.Fatalf("admitted Enqueue() error = %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("close did not finish after enqueue returned")
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "late"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("late Enqueue() error = %v", err)
	}
}

type countingEnqueueBackend struct{ calls atomic.Int32 }

func (b *countingEnqueueBackend) EnqueueContext(context.Context, *hibiken.Task, ...hibiken.Option) (*hibiken.TaskInfo, error) {
	b.calls.Add(1)
	return &hibiken.TaskInfo{ID: "id", Queue: "default", Type: "task"}, nil
}

type captureEnqueueBackend struct {
	task *hibiken.Task
	err  error
}

func (b *captureEnqueueBackend) EnqueueContext(_ context.Context, task *hibiken.Task, _ ...hibiken.Option) (*hibiken.TaskInfo, error) {
	b.task = task
	return nil, b.err
}

type blockingEnqueueBackend struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingEnqueueBackend) EnqueueContext(_ context.Context, task *hibiken.Task, _ ...hibiken.Option) (*hibiken.TaskInfo, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return &hibiken.TaskInfo{ID: "id", Queue: "default", Type: task.Type()}, nil
}

func TestClientCancellationBeforeAdmissionSkipsBackend(t *testing.T) {
	backend := &countingEnqueueBackend{}
	client := newClient(backend, defaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringValidation := taskOptionFunc(func(*taskOptions) { cancel() })

	if _, err := client.Enqueue(ctx, Task{Type: "canceled"}, cancelDuringValidation); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enqueue() error = %v, want context.Canceled", err)
	}
	if calls := backend.calls.Load(); calls != 0 {
		t.Fatalf("backend calls = %d, want 0", calls)
	}
}

type nilInfoEnqueueBackend struct{}

func (*nilInfoEnqueueBackend) EnqueueContext(context.Context, *hibiken.Task, ...hibiken.Option) (*hibiken.TaskInfo, error) {
	return nil, nil
}

func TestClientRejectsEmptySuccessfulBackendResponse(t *testing.T) {
	client := newClient(&nilInfoEnqueueBackend{}, defaultConfig())
	if _, err := client.Enqueue(context.Background(), Task{Type: "empty-response"}); err == nil {
		t.Fatal("Enqueue() error = nil")
	}
}
