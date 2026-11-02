package asynq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"

	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

var (
	// ErrQueueNotFound reports an operation on a queue Redis does not know.
	ErrQueueNotFound = errors.New("asynq: queue not found")
	// ErrTaskNotFound reports a task that is absent from the queue, or, for
	// CancelProcessing, absent from every queue this plugin declares.
	ErrTaskNotFound = errors.New("asynq: task not found")
	// ErrQueueNotEmpty reports an operation that requires an empty queue. The
	// queue library reports it for queue deletion, which this contract does not
	// expose; the sentinel is part of the error vocabulary for callers that
	// translate a vendor failure wholesale.
	ErrQueueNotEmpty = errors.New("asynq: queue is not empty")
	// ErrQueueNotOwned reports a queue this plugin does not declare under
	// plugins.asynq, in the top-level queues or in any workload's. Inspection and
	// control are scoped to the declared set so a management surface cannot
	// operate another process's queues by mistake.
	ErrQueueNotOwned = errors.New("asynq: queue is not declared by this plugin")
	// ErrInvalidState reports a state outside the six defined task states.
	ErrInvalidState = errors.New("asynq: invalid task state")
	// ErrUnsupportedState reports a defined state the operation does not support,
	// such as a bulk delete of active tasks.
	ErrUnsupportedState = errors.New("asynq: state is not supported by this operation")
)

// State is the state of a task. The six values below correspond one-to-one to
// the queue library's task states; StateUnknown is the zero value and reports a
// state outside that vocabulary, such as an aggregating task.
type State uint8

const (
	StateUnknown State = iota
	StatePending
	StateActive
	StateScheduled
	StateRetry
	StateArchived
	StateCompleted
)

// String returns the state's queue-library spelling, or "unknown".
func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateActive:
		return "active"
	case StateScheduled:
		return "scheduled"
	case StateRetry:
		return "retry"
	case StateArchived:
		return "archived"
	case StateCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// DefaultPageSize is the page size ListTasks uses when Page.Size is not
// positive. It matches the queue library's default.
const DefaultPageSize = 30

// Page selects one page of a task listing. The zero value selects the first
// page with DefaultPageSize. A Number below one is treated as one, and a Size
// that is not positive as DefaultPageSize.
type Page struct {
	Number int
	Size   int
}

func (p Page) normalized() (number, size int) {
	number, size = p.Number, p.Size
	if number < 1 {
		number = 1
	}
	if size <= 0 {
		size = DefaultPageSize
	}
	return number, size
}

// QueueInfo is a point-in-time count of one queue. Size is the sum of the six
// state counts. The counts are read directly from the queue library's Redis
// keys rather than through its GetQueueInfo, which samples MEMORY USAGE for
// every task and is too expensive for a management listing.
type QueueInfo struct {
	Queue     string
	Pending   int
	Active    int
	Scheduled int
	Retry     int
	Archived  int
	Completed int
	Size      int
	Paused    bool
}

// Inspector is the read-only half of queue management. Every method is scoped
// to the queues the plugin declares under plugins.asynq and reports
// ErrQueueNotOwned for any other name; after Stop every method reports
// ErrClosed.
type Inspector interface {
	// Queues returns the queue names this plugin declares, sorted. It reports
	// the configured set, not a Redis probe, so a declared queue appears even
	// before it has been used.
	Queues() ([]string, error)
	// QueueInfo reports one queue's state counts. It reports ErrQueueNotFound
	// for a declared queue Redis has never registered.
	QueueInfo(queue string) (QueueInfo, error)
	// ListTasks lists one page of a queue's tasks in one state. It reports
	// ErrInvalidState for a value outside the six states.
	ListTasks(queue string, state State, page Page) ([]TaskInfo, error)
	// GetTaskInfo reports one task. It reports ErrTaskNotFound when the task is
	// absent from the queue.
	GetTaskInfo(queue, id string) (TaskInfo, error)
}

// Redis key layout the queue library owns. QueueInfo and PurgeArchived read it
// directly for the one datum the library's public Inspector does not expose:
// every archived task's archive time, which is the archived sorted set's score.
// The layout is stable across the library's v0 minor releases; a major upgrade
// that changes it must be accompanied by a change here.
const (
	allQueuesKey   = "asynq:queues"
	queueKeyFormat = "asynq:{%s}:"
)

func queueKey(queue, suffix string) string {
	return fmt.Sprintf(queueKeyFormat, queue) + suffix
}

// authorizeQueue enforces the declared-queue scope.
func (p *Plugin) authorizeQueue(queue string) error {
	if _, ok := p.queues[queue]; ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrQueueNotOwned, queue)
}

// declaredQueueNames returns the declared queue set as a sorted slice.
func (p *Plugin) declaredQueueNames() []string {
	names := make([]string, 0, len(p.queues))
	for name := range p.queues {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// backends returns the owned Redis client and inspector, or ErrClosed once the
// plugin has stopped. Both share the plugin's one Redis connection.
func (p *Plugin) backends() (*goredis.Client, *hibiken.Inspector, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.redis == nil || p.inspector == nil {
		return nil, nil, ErrClosed
	}
	return p.redis, p.inspector, nil
}

// mapInspectionError translates the queue library's not-found errors into this
// package's sentinels while keeping the queue name in the message.
func mapInspectionError(err error, queue string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hibiken.ErrQueueNotFound):
		return fmt.Errorf("%w: %s", ErrQueueNotFound, queue)
	case errors.Is(err, hibiken.ErrTaskNotFound):
		return fmt.Errorf("%w in queue %s", ErrTaskNotFound, queue)
	case errors.Is(err, hibiken.ErrQueueNotEmpty):
		return fmt.Errorf("%w: %s", ErrQueueNotEmpty, queue)
	default:
		return err
	}
}

// Queues implements Inspector.
func (p *Plugin) Queues() ([]string, error) {
	if _, _, err := p.backends(); err != nil {
		return nil, err
	}
	return p.declaredQueueNames(), nil
}

// QueueInfo implements Inspector.
func (p *Plugin) QueueInfo(queue string) (QueueInfo, error) {
	if err := p.authorizeQueue(queue); err != nil {
		return QueueInfo{}, err
	}
	redisClient, _, err := p.backends()
	if err != nil {
		return QueueInfo{}, err
	}

	ctx := context.Background()
	pipe := redisClient.Pipeline()
	exists := pipe.SIsMember(ctx, allQueuesKey, queue)
	pending := pipe.LLen(ctx, queueKey(queue, "pending"))
	active := pipe.LLen(ctx, queueKey(queue, "active"))
	scheduled := pipe.ZCard(ctx, queueKey(queue, "scheduled"))
	retry := pipe.ZCard(ctx, queueKey(queue, "retry"))
	archived := pipe.ZCard(ctx, queueKey(queue, "archived"))
	completed := pipe.ZCard(ctx, queueKey(queue, "completed"))
	paused := pipe.Exists(ctx, queueKey(queue, "paused"))
	if _, err := pipe.Exec(ctx); err != nil {
		return QueueInfo{}, fmt.Errorf("asynq: read queue %q: %w", queue, err)
	}
	if !exists.Val() {
		return QueueInfo{}, fmt.Errorf("%w: %s", ErrQueueNotFound, queue)
	}

	info := QueueInfo{
		Queue:     queue,
		Pending:   int(pending.Val()),
		Active:    int(active.Val()),
		Scheduled: int(scheduled.Val()),
		Retry:     int(retry.Val()),
		Archived:  int(archived.Val()),
		Completed: int(completed.Val()),
		Paused:    paused.Val() > 0,
	}
	info.Size = info.Pending + info.Active + info.Scheduled + info.Retry + info.Archived + info.Completed
	return info, nil
}

// ListTasks implements Inspector.
func (p *Plugin) ListTasks(queue string, state State, page Page) ([]TaskInfo, error) {
	if err := p.authorizeQueue(queue); err != nil {
		return nil, err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return nil, err
	}
	number, size := page.normalized()
	options := []hibiken.ListOption{hibiken.PageSize(size), hibiken.Page(number)}

	var listing []*hibiken.TaskInfo
	switch state {
	case StatePending:
		listing, err = inspector.ListPendingTasks(queue, options...)
	case StateActive:
		listing, err = inspector.ListActiveTasks(queue, options...)
	case StateScheduled:
		listing, err = inspector.ListScheduledTasks(queue, options...)
	case StateRetry:
		listing, err = inspector.ListRetryTasks(queue, options...)
	case StateArchived:
		listing, err = inspector.ListArchivedTasks(queue, options...)
	case StateCompleted:
		listing, err = inspector.ListCompletedTasks(queue, options...)
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidState, state)
	}
	if err != nil {
		return nil, mapInspectionError(err, queue)
	}

	tasks := make([]TaskInfo, 0, len(listing))
	for _, item := range listing {
		tasks = append(tasks, taskInfoFromVendor(item))
	}
	return tasks, nil
}

// GetTaskInfo implements Inspector.
func (p *Plugin) GetTaskInfo(queue, id string) (TaskInfo, error) {
	if err := p.authorizeQueue(queue); err != nil {
		return TaskInfo{}, err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return TaskInfo{}, err
	}
	info, err := inspector.GetTaskInfo(queue, id)
	if err != nil {
		return TaskInfo{}, mapInspectionError(err, queue)
	}
	return taskInfoFromVendor(info), nil
}

// stateFromVendor maps the queue library's state onto this package's enum.
func stateFromVendor(state hibiken.TaskState) State {
	switch state {
	case hibiken.TaskStatePending:
		return StatePending
	case hibiken.TaskStateActive:
		return StateActive
	case hibiken.TaskStateScheduled:
		return StateScheduled
	case hibiken.TaskStateRetry:
		return StateRetry
	case hibiken.TaskStateArchived:
		return StateArchived
	case hibiken.TaskStateCompleted:
		return StateCompleted
	default:
		return StateUnknown
	}
}

// taskInfoFromVendor copies one vendor task description into the neutral value.
// Payload and headers are copied so a caller cannot alias library-owned memory.
func taskInfoFromVendor(info *hibiken.TaskInfo) TaskInfo {
	if info == nil {
		return TaskInfo{}
	}
	return TaskInfo{
		ID:            info.ID,
		Queue:         info.Queue,
		Type:          info.Type,
		Payload:       append([]byte(nil), info.Payload...),
		Headers:       maps.Clone(info.Headers),
		State:         stateFromVendor(info.State),
		MaxRetries:    info.MaxRetry,
		Retried:       info.Retried,
		LastErr:       info.LastErr,
		LastFailedAt:  info.LastFailedAt,
		Timeout:       info.Timeout,
		Deadline:      info.Deadline,
		NextProcessAt: info.NextProcessAt,
		IsOrphaned:    info.IsOrphaned,
		Retention:     info.Retention,
		CompletedAt:   info.CompletedAt,
	}
}
