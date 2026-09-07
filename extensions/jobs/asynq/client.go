package asynq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	hibiken "github.com/hibiken/asynq"
)

// TaskOption configures one enqueue operation without exposing Asynq's API.
// Values are created with Queue, MaxRetries, Timeout, Deadline, ProcessAt,
// ProcessIn, UniqueFor, TaskID, and Retention.
type TaskOption interface {
	apply(*taskOptions)
}

type taskOptionFunc func(*taskOptions)

func (f taskOptionFunc) apply(options *taskOptions) { f(options) }

type scheduleMode uint8

const (
	scheduleNow scheduleMode = iota
	scheduleAt
	scheduleIn
)

type taskOptions struct {
	queue      string
	maxRetries int
	timeout    time.Duration
	deadline   time.Time
	schedule   scheduleMode
	processAt  time.Time
	processIn  time.Duration
	uniqueFor  time.Duration
	taskID     string
	retention  time.Duration
}

// Queue selects one configured queue for this task.
func Queue(name string) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.queue = name })
}

// MaxRetries overrides the configured retry count. Zero disables retries.
func MaxRetries(count int) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.maxRetries = count })
}

// Timeout limits one handler attempt.
func Timeout(timeout time.Duration) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.timeout = timeout })
}

// Deadline sets an absolute processing deadline.
func Deadline(deadline time.Time) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.deadline = deadline })
}

// ProcessAt schedules the task for an absolute time. It overrides an earlier
// ProcessIn option, and a later ProcessIn overrides it.
func ProcessAt(at time.Time) TaskOption {
	return taskOptionFunc(func(options *taskOptions) {
		options.schedule = scheduleAt
		options.processAt = at
	})
}

// ProcessIn schedules the task after delay. It overrides an earlier ProcessAt
// option, and a later ProcessAt overrides it.
func ProcessIn(delay time.Duration) TaskOption {
	return taskOptionFunc(func(options *taskOptions) {
		options.schedule = scheduleIn
		options.processIn = delay
	})
}

// UniqueFor prevents another task with the same type, payload, and queue from
// being enqueued during ttl. The underlying queue requires at least one second.
func UniqueFor(ttl time.Duration) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.uniqueFor = ttl })
}

// TaskID assigns a caller-controlled task identifier.
func TaskID(id string) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.taskID = id })
}

// Retention keeps a successful task's result metadata for duration.
func Retention(duration time.Duration) TaskOption {
	return taskOptionFunc(func(options *taskOptions) { options.retention = duration })
}

type enqueueBackend interface {
	EnqueueContext(context.Context, *hibiken.Task, ...hibiken.Option) (*hibiken.TaskInfo, error)
}

// Client is a transport-neutral, concurrently safe enqueue client. Shutdown
// waits for already-admitted Enqueue calls before closing Redis and rejects all
// later calls with ErrClosed.
type Client struct {
	mu       sync.Mutex
	backend  enqueueBackend
	closed   bool
	inflight sync.WaitGroup

	queues   map[string]struct{}
	defaults taskOptions
}

var _ Enqueuer = (*Client)(nil)

func newClient(backend enqueueBackend, cfg Config) *Client {
	queues := make(map[string]struct{}, len(cfg.Queues))
	for name := range cfg.Queues {
		queues[name] = struct{}{}
	}
	return &Client{
		backend: backend,
		queues:  queues,
		defaults: taskOptions{
			queue:      cfg.DefaultQueue,
			maxRetries: cfg.DefaultMaxRetries,
			timeout:    cfg.DefaultTimeout,
		},
	}
}

// Enqueue persists task in Redis using the configured defaults plus options.
func (c *Client) Enqueue(ctx context.Context, task Task, options ...TaskOption) (TaskInfo, error) {
	if ctx == nil {
		return TaskInfo{}, fmt.Errorf("asynq: enqueue requires a non-nil context")
	}
	if err := ctx.Err(); err != nil {
		return TaskInfo{}, err
	}
	if strings.TrimSpace(task.Type) != task.Type || task.Type == "" {
		return TaskInfo{}, fmt.Errorf("asynq: task type must be non-empty and have no surrounding whitespace")
	}
	for name := range task.Headers {
		if strings.TrimSpace(name) != name || name == "" {
			return TaskInfo{}, fmt.Errorf("asynq: task header name %q must be non-empty and have no surrounding whitespace", name)
		}
	}

	settings := c.defaults
	for index, option := range options {
		if option == nil {
			return TaskInfo{}, fmt.Errorf("asynq: task option %d is nil", index)
		}
		option.apply(&settings)
	}
	if err := c.validateOptions(settings); err != nil {
		return TaskInfo{}, err
	}

	vendorOptions := []hibiken.Option{
		hibiken.Queue(settings.queue),
		hibiken.MaxRetry(settings.maxRetries),
		hibiken.Timeout(settings.timeout),
	}
	if !settings.deadline.IsZero() {
		vendorOptions = append(vendorOptions, hibiken.Deadline(settings.deadline))
	}
	switch settings.schedule {
	case scheduleAt:
		vendorOptions = append(vendorOptions, hibiken.ProcessAt(settings.processAt))
	case scheduleIn:
		vendorOptions = append(vendorOptions, hibiken.ProcessIn(settings.processIn))
	}
	if settings.uniqueFor != 0 {
		vendorOptions = append(vendorOptions, hibiken.Unique(settings.uniqueFor))
	}
	if settings.taskID != "" {
		vendorOptions = append(vendorOptions, hibiken.TaskID(settings.taskID))
	}
	if settings.retention != 0 {
		vendorOptions = append(vendorOptions, hibiken.Retention(settings.retention))
	}

	vendorTask := hibiken.NewTaskWithHeaders(task.Type, append([]byte(nil), task.Payload...), maps.Clone(task.Headers))

	c.mu.Lock()
	if c.closed || c.backend == nil {
		c.mu.Unlock()
		return TaskInfo{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return TaskInfo{}, err
	}
	backend := c.backend
	c.inflight.Add(1)
	c.mu.Unlock()
	defer c.inflight.Done()

	info, err := backend.EnqueueContext(ctx, vendorTask, vendorOptions...)
	if err != nil {
		switch {
		case errors.Is(err, hibiken.ErrDuplicateTask):
			return TaskInfo{}, fmt.Errorf("%w: type %q in queue %q", ErrDuplicateTask, task.Type, settings.queue)
		case errors.Is(err, hibiken.ErrTaskIDConflict):
			return TaskInfo{}, fmt.Errorf("%w: %q", ErrTaskIDConflict, settings.taskID)
		default:
			return TaskInfo{}, fmt.Errorf("asynq: enqueue task %q in queue %q: %w", task.Type, settings.queue, err)
		}
	}
	if info == nil {
		return TaskInfo{}, fmt.Errorf("asynq: enqueue task %q in queue %q returned no task information", task.Type, settings.queue)
	}
	return TaskInfo{
		ID:            info.ID,
		Queue:         info.Queue,
		Type:          info.Type,
		MaxRetries:    info.MaxRetry,
		Timeout:       info.Timeout,
		NextProcessAt: info.NextProcessAt,
	}, nil
}

func (c *Client) validateOptions(options taskOptions) error {
	if strings.TrimSpace(options.queue) != options.queue || options.queue == "" {
		return fmt.Errorf("asynq: task queue must be non-empty and have no surrounding whitespace")
	}
	if _, ok := c.queues[options.queue]; !ok {
		return fmt.Errorf("asynq: task queue %q is not configured", options.queue)
	}
	if options.maxRetries < 0 {
		return fmt.Errorf("asynq: task max retries cannot be negative, got %d", options.maxRetries)
	}
	if options.timeout <= 0 {
		return fmt.Errorf("asynq: task timeout must be positive, got %s", options.timeout)
	}
	if !options.deadline.IsZero() && options.deadline.Unix() <= 0 {
		return fmt.Errorf("asynq: task deadline must be after the Unix epoch")
	}
	switch options.schedule {
	case scheduleNow:
	case scheduleAt:
		if options.processAt.IsZero() {
			return fmt.Errorf("asynq: task process time cannot be zero")
		}
	case scheduleIn:
		if options.processIn < 0 {
			return fmt.Errorf("asynq: task process delay cannot be negative, got %s", options.processIn)
		}
	default:
		return fmt.Errorf("asynq: invalid task schedule mode %d", options.schedule)
	}
	if options.uniqueFor != 0 && options.uniqueFor < time.Second {
		return fmt.Errorf("asynq: task unique duration must be at least 1s, got %s", options.uniqueFor)
	}
	if options.taskID != "" && (strings.TrimSpace(options.taskID) != options.taskID) {
		return fmt.Errorf("asynq: task ID must have no surrounding whitespace")
	}
	if options.retention < 0 {
		return fmt.Errorf("asynq: task retention cannot be negative, got %s", options.retention)
	}
	return nil
}

// beginClose atomically closes admission without waiting for calls that were
// already admitted. Adding to inflight and closing admission are serialized by
// mu, so once this method returns Wait cannot race a later Add.
func (c *Client) beginClose() {
	c.mu.Lock()
	c.closed = true
	c.backend = nil
	c.mu.Unlock()
}

func (c *Client) close() {
	c.beginClose()
	c.inflight.Wait()
}
