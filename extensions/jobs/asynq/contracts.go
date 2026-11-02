package asynq

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrClosed reports an enqueue attempted after plugin shutdown began.
	ErrClosed = errors.New("asynq: enqueue client is closed")
	// ErrDuplicateTask reports that a task protected by UniqueFor already exists.
	ErrDuplicateTask = errors.New("asynq: duplicate task")
	// ErrTaskIDConflict reports reuse of an existing explicit task ID.
	ErrTaskIDConflict = errors.New("asynq: task ID conflict")
	// ErrHandlerNotFound reports delivery of a task type with no registered handler.
	ErrHandlerNotFound = errors.New("asynq: task handler not found")
	// ErrClusterConcurrencyLimit reports that a task type's cluster-wide
	// concurrency limit is full, so the delivery was deferred rather than run.
	// It is returned wrapped in RetryAfter, so the retry delay is the one
	// configured by cluster_concurrency_retry_delay.
	ErrClusterConcurrencyLimit = errors.New("asynq: cluster concurrency limit reached")
)

// Task is the transport-neutral task representation used for both enqueueing
// and handling. Implementations must treat Payload and Headers as immutable.
type Task struct {
	Type    string
	Payload []byte
	Headers map[string]string
}

// TaskInfo describes a task known to Redis without exposing Asynq types.
// Enqueue fills the fields it can know from the submission; inspection fills
// the rest. State is StateUnknown on a value returned by Enqueue.
type TaskInfo struct {
	ID         string
	Queue      string
	Type       string
	Payload    []byte
	Headers    map[string]string
	State      State
	MaxRetries int
	Retried    int
	LastErr    string
	// LastFailedAt is the time of the task's last handler failure, zero if it
	// never failed. It is not the archive time; use PurgeArchived's olderThan
	// to select archived tasks by when they were archived.
	LastFailedAt  time.Time
	Timeout       time.Duration
	Deadline      time.Time
	NextProcessAt time.Time
	// IsOrphaned reports an active task whose worker lease expired. It is only
	// meaningful for StateActive.
	IsOrphaned  bool
	Retention   time.Duration
	CompletedAt time.Time
}

// Enqueuer is the narrow dependency business plugins normally consume.
type Enqueuer interface {
	Enqueue(context.Context, Task, ...TaskOption) (TaskInfo, error)
}

// Handler processes one task. Returning an error asks the worker to apply the
// task's configured retry policy.
type Handler interface {
	HandleTask(context.Context, Task) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Task) error

func (f HandlerFunc) HandleTask(ctx context.Context, task Task) error {
	return f(ctx, task)
}

// HandlerRegistration binds one exact task type to one Handler.
type HandlerRegistration struct {
	Type    string
	Handler Handler
}

// HandlerContributor contributes task handlers to the Asynq Definition. Producers
// export this contract explicitly; Asynq collects the typed input in graph order
// and freezes each contributor's registrations during construction.
//
// A contributor's Plugin may declare a workload. The handlers then run on that
// workload's worker, under its queues, concurrency, and admission quota, which
// is how a shared integration serves several workloads without any of them
// escaping the limits it declared. A contributor that declares none is served
// by the top-level worker.
type HandlerContributor interface {
	TaskHandlers() []HandlerRegistration
}
