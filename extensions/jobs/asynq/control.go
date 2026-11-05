package asynq

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

// purgeBatchSize bounds how many archived tasks one PurgeArchived round loads
// before deleting them, so a queue with a large archive is cleaned in steps
// rather than one unbounded read.
const purgeBatchSize = 500

// semaphoreResetTimeout caps one ResetSemaphore Redis operation; a caller's
// context with an earlier deadline still wins.
const semaphoreResetTimeout = 5 * time.Second

// Controller is the destructive half of queue management: it deletes, moves,
// archives, cancels, pauses, and purges. It is exported as a separate contract
// from Inspector so a read-only consumer cannot accidentally receive the
// ability to mutate a queue.
//
// Authorization is the caller's responsibility. This contract performs no
// authentication or access control check beyond the declared-queue scope; a
// Plugin that exposes it through an operator endpoint must enforce its own
// permission model there.
//
// Every method is scoped to the queues the plugin declares under plugins.asynq
// and reports ErrQueueNotOwned for any other name; after Stop every method
// reports ErrClosed.
type Controller interface {
	// DeleteTask deletes one pending, scheduled, retry, or archived task.
	DeleteTask(queue, id string) error
	// DeleteAllByState deletes every task in one state and reports how many it
	// deleted. Pending, scheduled, retry, archived, and completed are
	// supported; active reports ErrUnsupportedState because a running task must
	// be cancelled first.
	DeleteAllByState(queue string, state State) (int, error)
	// RunTask moves a scheduled, retry, or archived task to pending.
	RunTask(queue, id string) error
	// ArchiveTask moves a pending, scheduled, or retry task to archived.
	ArchiveTask(queue, id string) error
	// CancelProcessing asks the worker running taskID to cancel it. The queue
	// library's cancellation is best-effort and the signal has no queue
	// argument, so the task is first located in the declared queues; a task found
	// in none reports ErrTaskNotFound and no signal is published.
	CancelProcessing(taskID string) error
	// PauseQueue stops the queue's workers from fetching new tasks. Pausing an
	// already paused queue is an error.
	PauseQueue(queue string) error
	// UnpauseQueue resumes a paused queue. Unpausing a queue that is not paused
	// is an error.
	UnpauseQueue(queue string) error
	// PurgeArchived deletes archived tasks whose archive time is at or before
	// olderThan and reports how many it deleted. The zero time purges every
	// archived task. With no taskTypes every type is purged; with one or more,
	// only archived tasks whose type equals one of them are deleted, and each
	// considered task's metadata is read to learn its type. Tasks are deleted
	// in batches of purgeBatchSize.
	PurgeArchived(queue string, olderThan time.Time, taskTypes ...string) (int, error)
	// ResetSemaphore removes expired slots from the cluster-wide concurrency
	// semaphore of taskType and reports nothing about live slots, which it
	// leaves untouched. It is safe during a rolling restart; a task type that
	// holds no semaphore key succeeds without creating one.
	ResetSemaphore(ctx context.Context, taskType string) error
}

// DeleteTask implements Controller.
func (p *Plugin) DeleteTask(queue, id string) error {
	if err := p.authorizeQueue(queue); err != nil {
		return err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	return mapInspectionError(inspector.DeleteTask(queue, id), queue)
}

// DeleteAllByState implements Controller.
func (p *Plugin) DeleteAllByState(queue string, state State) (int, error) {
	if err := p.authorizeQueue(queue); err != nil {
		return 0, err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return 0, err
	}

	var deleted int
	switch state {
	case StatePending:
		deleted, err = inspector.DeleteAllPendingTasks(queue)
	case StateScheduled:
		deleted, err = inspector.DeleteAllScheduledTasks(queue)
	case StateRetry:
		deleted, err = inspector.DeleteAllRetryTasks(queue)
	case StateArchived:
		deleted, err = inspector.DeleteAllArchivedTasks(queue)
	case StateCompleted:
		deleted, err = inspector.DeleteAllCompletedTasks(queue)
	case StateActive:
		return 0, fmt.Errorf("%w: active tasks must be cancelled one at a time", ErrUnsupportedState)
	default:
		return 0, fmt.Errorf("%w: %s", ErrInvalidState, state)
	}
	if err != nil {
		return 0, mapInspectionError(err, queue)
	}
	return deleted, nil
}

// RunTask implements Controller.
func (p *Plugin) RunTask(queue, id string) error {
	if err := p.authorizeQueue(queue); err != nil {
		return err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	return mapInspectionError(inspector.RunTask(queue, id), queue)
}

// ArchiveTask implements Controller.
func (p *Plugin) ArchiveTask(queue, id string) error {
	if err := p.authorizeQueue(queue); err != nil {
		return err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	return mapInspectionError(inspector.ArchiveTask(queue, id), queue)
}

// CancelProcessing implements Controller.
func (p *Plugin) CancelProcessing(taskID string) error {
	if taskID == "" || strings.TrimSpace(taskID) != taskID {
		return fmt.Errorf("%w: invalid task id %q", ErrTaskNotFound, taskID)
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	for _, queue := range p.declaredQueueNames() {
		if _, lookupErr := inspector.GetTaskInfo(queue, taskID); lookupErr != nil {
			if errors.Is(lookupErr, hibiken.ErrTaskNotFound) || errors.Is(lookupErr, hibiken.ErrQueueNotFound) {
				continue
			}
			return fmt.Errorf("asynq: locate task %q: %w", taskID, lookupErr)
		}
		if err := inspector.CancelProcessing(taskID); err != nil {
			return fmt.Errorf("asynq: cancel task %q in queue %q: %w", taskID, queue, err)
		}
		return nil
	}
	return fmt.Errorf("%w: %q in any queue this plugin declares", ErrTaskNotFound, taskID)
}

// PauseQueue implements Controller.
func (p *Plugin) PauseQueue(queue string) error {
	if err := p.authorizeQueue(queue); err != nil {
		return err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	if err := inspector.PauseQueue(queue); err != nil {
		return fmt.Errorf("asynq: pause queue %q: %w", queue, err)
	}
	return nil
}

// UnpauseQueue implements Controller.
func (p *Plugin) UnpauseQueue(queue string) error {
	if err := p.authorizeQueue(queue); err != nil {
		return err
	}
	_, inspector, err := p.backends()
	if err != nil {
		return err
	}
	if err := inspector.UnpauseQueue(queue); err != nil {
		return fmt.Errorf("asynq: unpause queue %q: %w", queue, err)
	}
	return nil
}

// PurgeArchived implements Controller.
//
// The archive time is the archived sorted set's score, which is what the queue
// library writes when a task is archived, including when it is archived through
// the Inspector rather than by a failed handler. TaskInfo.LastFailedAt cannot
// be used instead: it records the last handler failure and is zero or stale for
// tasks archived by the Inspector, so a purge keyed on it would delete new
// archived tasks as if they were old.
//
// A task type filter turns the purge into a metadata read per considered task:
// the sorted set carries no type, so GetTaskInfo decodes the stored task to
// learn it. Without a filter this method reads no task metadata at all.
func (p *Plugin) PurgeArchived(queue string, olderThan time.Time, taskTypes ...string) (int, error) {
	if err := p.authorizeQueue(queue); err != nil {
		return 0, err
	}
	types := make(map[string]struct{}, len(taskTypes))
	for _, taskType := range taskTypes {
		if taskType == "" || strings.TrimSpace(taskType) != taskType {
			return 0, fmt.Errorf("asynq: invalid task type %q", taskType)
		}
		types[taskType] = struct{}{}
	}
	redisClient, inspector, err := p.backends()
	if err != nil {
		return 0, err
	}

	ctx := context.Background()
	exists, err := redisClient.SIsMember(ctx, allQueuesKey, queue).Result()
	if err != nil {
		return 0, fmt.Errorf("asynq: locate queue %q: %w", queue, err)
	}
	if !exists {
		return 0, fmt.Errorf("%w: %s", ErrQueueNotFound, queue)
	}

	max := "+inf"
	if !olderThan.IsZero() {
		max = strconv.FormatInt(olderThan.Unix(), 10)
	}
	archivedKey := queueKey(queue, "archived")

	purged := 0
	// retained counts the archived tasks in the score range that the type
	// filter keeps. A non-matching task stays in the range, so reading the next
	// batch at a fixed offset would return the same leading tasks forever; using
	// the retained count as the offset moves the window past them. Deleted
	// matches and removed orphans no longer occupy the range, so only retained
	// entries can precede the next batch.
	retained := 0
	for {
		ids, err := redisClient.ZRangeByScore(ctx, archivedKey, &goredis.ZRangeBy{
			Min:    "-inf",
			Max:    max,
			Offset: int64(retained),
			Count:  purgeBatchSize,
		}).Result()
		if err != nil {
			return purged, fmt.Errorf("asynq: list archived tasks of queue %q: %w", queue, err)
		}
		if len(ids) == 0 {
			return purged, nil
		}
		for _, id := range ids {
			if len(types) > 0 {
				info, infoErr := inspector.GetTaskInfo(queue, id)
				switch {
				case infoErr != nil && errors.Is(infoErr, hibiken.ErrTaskNotFound):
					// An orphan archive entry under a type filter: it names a task
					// whose hash is gone, so it matches no type and must be removed
					// or it would stay in the range for every later batch.
					if _, zerr := redisClient.ZRem(ctx, archivedKey, id).Result(); zerr != nil {
						return purged, fmt.Errorf("asynq: remove orphan archived task %q of queue %q: %w", id, queue, zerr)
					}
					continue
				case infoErr != nil:
					return purged, fmt.Errorf("asynq: inspect archived task %q of queue %q: %w", id, queue, mapInspectionError(infoErr, queue))
				}
				if _, ok := types[info.Type]; !ok {
					retained++
					continue
				}
			}
			switch deleteErr := inspector.DeleteTask(queue, id); {
			case deleteErr == nil:
				purged++
			case errors.Is(deleteErr, hibiken.ErrTaskNotFound):
				// An orphan archive entry: the sorted set names a task whose hash
				// is gone. Remove the member so the next batch makes progress.
				if _, zerr := redisClient.ZRem(ctx, archivedKey, id).Result(); zerr != nil {
					return purged, fmt.Errorf("asynq: remove orphan archived task %q of queue %q: %w", id, queue, zerr)
				}
			default:
				return purged, fmt.Errorf("asynq: delete archived task %q of queue %q: %w", id, queue, mapInspectionError(deleteErr, queue))
			}
		}
	}
}

// ResetSemaphore implements Controller.
func (p *Plugin) ResetSemaphore(ctx context.Context, taskType string) error {
	if ctx == nil {
		return fmt.Errorf("asynq: reset semaphore requires a non-nil context")
	}
	if taskType == "" || strings.TrimSpace(taskType) != taskType {
		return fmt.Errorf("asynq: invalid task type %q", taskType)
	}
	p.mu.Lock()
	semaphore := p.semaphore
	p.mu.Unlock()
	if semaphore == nil {
		return ErrClosed
	}
	resetCtx, cancel := context.WithTimeout(ctx, semaphoreResetTimeout)
	defer cancel()
	if _, err := semaphore.Reset(resetCtx, taskType); err != nil {
		return fmt.Errorf("asynq: reset cluster semaphore for task type %q: %w", taskType, err)
	}
	return nil
}
