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
)

// Task is the transport-neutral task representation used for both enqueueing
// and handling. Implementations must treat Payload and Headers as immutable.
type Task struct {
	Type    string
	Payload []byte
	Headers map[string]string
}

// TaskInfo describes a task accepted by Redis without exposing Asynq types.
type TaskInfo struct {
	ID            string
	Queue         string
	Type          string
	MaxRetries    int
	Timeout       time.Duration
	NextProcessAt time.Time
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
type HandlerContributor interface {
	TaskHandlers() []HandlerRegistration
}
