package asynq

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"runtime/pprof"
	"strings"

	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

// workloadProfileLabel is the profiler label key a task is filed under, and it
// is deliberately the same word the runtime files its managed tasks under
// (runtime/task.go): a profile consumer filters by key, so two names for one
// concept would split the attribution a reader is trying to join. The literal
// in this package's own tests pins the spelling.
const workloadProfileLabel = "workload"

type dispatcher struct {
	// workload is the group this dispatcher serves, and "" for the group of
	// contributors that belong to none.
	workload plugin.WorkloadKey
	handlers map[string]Handler
}

func (d *dispatcher) ProcessTask(ctx context.Context, task *hibiken.Task) error {
	handler, ok := d.handlers[task.Type()]
	if !ok {
		return fmt.Errorf("%w for type %q", ErrHandlerNotFound, task.Type())
	}
	work := func(taskCtx context.Context) error {
		return handler.HandleTask(taskCtx, Task{
			Type:    task.Type(),
			Payload: append([]byte(nil), task.Payload()...),
			Headers: maps.Clone(task.Headers()),
		})
	}
	// A task arrives on a worker goroutine the queue library owns and reuses,
	// so this is where the workload can be attached to it: the label set
	// applies for exactly the duration of the handler and is restored
	// afterwards, which keeps one workload's task from filing the next task
	// the same goroutine picks up.
	//
	// The unowned group is left unlabelled, on the same argument the runtime
	// makes for unowned plugins: those contributors are the shared ones, and a
	// label naming no workload is the truth about them.
	if d.workload == "" {
		return work(ctx)
	}
	var err error
	pprof.Do(ctx, pprof.Labels(workloadProfileLabel, d.workload.String()), func(taskCtx context.Context) {
		err = work(taskCtx)
	})
	return err
}

// handlerGroup is one worker's handler set: the handlers contributed by the
// Plugins that share a workload, plus the workload they belong to (empty for
// the group of contributors that belong to none).
type handlerGroup struct {
	workload   plugin.WorkloadKey
	dispatcher *dispatcher
}

// groupHandlers splits the collected contributors into one handler set per
// workload, in the order the workloads first appear.
//
// A workload's Plugins are served by that workload's server, so its queue set,
// concurrency and admission quota are the workload's own; Plugins that belong
// to no workload -- the ones a standby process exists to run -- share the
// top-level worker. Splitting here rather than by running every handler through
// one server is what keeps "which queues does this process consume" tied to
// "which workloads does it host".
//
// An empty result is not an error. A process that contributes no handlers is a
// standby between two roles, or a process that composes the integration only to
// enqueue, and failing its construction was what made the takeover model
// unusable: the worker would have refused to exist exactly when a role change
// was about to be decided. The plugin idles instead (see Plugin.start).
//
// Duplicate registration is refused within a group, where it would make the
// handler a task gets depend on registration order. Across groups the same task
// type may appear more than once: the groups consume disjoint queues, so which
// handler runs is decided by the queue the task was enqueued to rather than by
// the order contributors were collected in.
func groupHandlers(contributors []plugin.Entry[HandlerContributor]) ([]handlerGroup, error) {
	var groups []handlerGroup
	index := make(map[plugin.WorkloadKey]int)
	handlers := make(map[plugin.WorkloadKey]map[string]Handler)
	owners := make(map[plugin.WorkloadKey]map[string]plugin.Identity)

	for _, contributor := range contributors {
		registrations, err := contributorHandlers(contributor)
		if err != nil {
			return nil, err
		}
		workload := contributor.Workload
		if _, known := index[workload]; !known {
			index[workload] = len(groups)
			groups = append(groups, handlerGroup{workload: workload, dispatcher: &dispatcher{workload: workload, handlers: map[string]Handler{}}})
			handlers[workload] = groups[len(groups)-1].dispatcher.handlers
			owners[workload] = make(map[string]plugin.Identity)
		}
		for position, registration := range registrations {
			if strings.TrimSpace(registration.Type) != registration.Type || registration.Type == "" {
				return nil, fmt.Errorf("asynq: HandlerContributor %s returned invalid task type %q at index %d%s", contributor.Identity, registration.Type, position, inWorkload(workload))
			}
			if isNilInterface(registration.Handler) {
				return nil, fmt.Errorf("asynq: HandlerContributor %s returned nil handler for type %q at index %d%s", contributor.Identity, registration.Type, position, inWorkload(workload))
			}
			if previous, duplicate := owners[workload][registration.Type]; duplicate {
				return nil, fmt.Errorf("asynq: duplicate handler for task type %q from %s and %s%s", registration.Type, previous, contributor.Identity, inWorkload(workload))
			}
			handlers[workload][registration.Type] = registration.Handler
			owners[workload][registration.Type] = contributor.Identity
		}
	}

	// A contributor that registered nothing leaves no group behind. A server
	// that consumes a queue set while holding no handler for the tasks on it
	// would fetch work it can only fail, so an empty handler set means the
	// workload has no worker here rather than an idle one.
	served := make([]handlerGroup, 0, len(groups))
	for _, group := range groups {
		if len(group.dispatcher.handlers) > 0 {
			served = append(served, group)
		}
	}
	return served, nil
}

// inWorkload names the workload a message is about, for errors that would
// otherwise read as if one server handled the whole process.
func inWorkload(workload plugin.WorkloadKey) string {
	if workload == "" {
		return " in the unowned group"
	}
	return fmt.Sprintf(" in workload %q", workload)
}

func contributorHandlers(contributor plugin.Entry[HandlerContributor]) (registrations []HandlerRegistration, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			registrations = nil
			err = fmt.Errorf("asynq: HandlerContributor %s TaskHandlers callback panicked: %v", contributor.Identity, recovered)
		}
	}()
	return contributor.Value.TaskHandlers(), nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
