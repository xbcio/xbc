package asynq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xbcio/xbc/extensions/tasks"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// taskProviders collects the tasks.Provider exports of the collected plugins,
// read once at construction. Collecting them places every providing plugin
// upstream of this Plugin in the dependency graph, so a remotely consumed
// handler's dependencies are still running while it runs.
var taskProviders = plugin.Collect[tasks.Provider]()

// taskBinding is one task this process consumes: a method binding collected
// from a providing plugin, or a function task this process consumes by
// configuration. It keeps the typed binding the tasks contract needs for Run,
// alongside the owner duplicate messages name and the workload whose worker
// serves it.
type taskBinding struct {
	binding  tasks.Binding
	owner    plugin.Identity
	workload plugin.WorkloadKey
}

// assemblePlugin builds the Plugin from the collected handler contributors and
// task providers: it groups both handler sources by workload, resolves every
// group into its worker, checks that each consumed task's queue is fetched by
// the worker that runs it, and freezes the per-task deployment settings the
// remote executor submits with.
func assemblePlugin(cfg Config, contributors []plugin.Entry[HandlerContributor], providers []plugin.Entry[tasks.Provider]) (*Plugin, error) {
	bindings, err := collectTaskBindings(cfg, providers)
	if err != nil {
		return nil, err
	}
	grouped, err := groupHandlers(contributors, bindings)
	if err != nil {
		return nil, err
	}
	groups, err := newWorkerGroups(cfg, grouped)
	if err != nil {
		return nil, err
	}
	if err := checkTaskQueues(cfg, bindings, groups); err != nil {
		return nil, err
	}
	return &Plugin{
		cfg:          cfg.clone(),
		factory:      defaultBackendFactory(),
		groups:       groups,
		taskBindings: taskLookup(bindings),
		taskOptions:  resolvedTaskOptions(cfg),
	}, nil
}

// collectTaskBindings returns every task this process consumes, in a
// deterministic order: the collected providers' bindings in collection order,
// then the function tasks explicitly configured for consumption in name order.
func collectTaskBindings(cfg Config, providers []plugin.Entry[tasks.Provider]) ([]taskBinding, error) {
	var bindings []taskBinding
	for _, provider := range providers {
		collected, err := providerTaskBindings(provider)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, collected...)
	}
	consumed, err := consumedFuncs(cfg)
	if err != nil {
		return nil, err
	}
	return append(bindings, consumed...), nil
}

// providerTaskBindings flattens one provider's bindings, checking each
// binding's shape where it is read so a malformed one fails construction with
// its provider named rather than surfacing later as a dispatch failure.
func providerTaskBindings(provider plugin.Entry[tasks.Provider]) ([]taskBinding, error) {
	bindings, err := providerBindings(provider)
	if err != nil {
		return nil, err
	}
	out := make([]taskBinding, 0, len(bindings))
	for index, binding := range bindings {
		name := binding.Name()
		if strings.TrimSpace(name) != name || name == "" {
			return nil, fmt.Errorf("asynq: tasks.Provider %s returned invalid task name %q at index %d%s", provider.Identity, name, index, inWorkload(provider.Workload))
		}
		if isNilInterface(binding.Handler()) {
			return nil, fmt.Errorf("asynq: tasks.Provider %s returned nil handler for task %q at index %d%s", provider.Identity, name, index, inWorkload(provider.Workload))
		}
		out = append(out, taskBinding{binding: binding, owner: provider.Identity, workload: provider.Workload})
	}
	return out, nil
}

// providerBindings calls the provider's Tasks method, turning a panic in it
// into a construction error instead of letting it escape construction, the
// way contributorHandlers does for HandlerContributor.
func providerBindings(provider plugin.Entry[tasks.Provider]) (bindings []tasks.Binding, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			bindings = nil
			err = fmt.Errorf("asynq: tasks.Provider %s Tasks callback panicked: %v", provider.Identity, recovered)
		}
	}()
	return provider.Value.Tasks(), nil
}

// consumedFuncs returns the function tasks this process is configured to
// consume: every tasks.New task named in plugins.asynq.tasks with consume
// true. They join the unowned worker, because a function task belongs to no
// contributing plugin and therefore to no workload.
//
// A name with no tasks.New definition is refused rather than ignored. Consume
// is an explicit statement that this process runs the task, and one nothing
// here can run -- a typo, or a tasks.Method task that its providing plugin
// already consumes -- would leave the stated intent unmet while the queue
// quietly backs up.
func consumedFuncs(cfg Config) ([]taskBinding, error) {
	var consuming []string
	for name, task := range cfg.Tasks {
		if task.Consume {
			consuming = append(consuming, name)
		}
	}
	if len(consuming) == 0 {
		return nil, nil
	}
	sort.Strings(consuming)

	defined := make(map[string]tasks.Binding)
	for _, binding := range tasks.Funcs() {
		defined[binding.Name()] = binding
	}
	out := make([]taskBinding, 0, len(consuming))
	for _, name := range consuming {
		binding, ok := defined[name]
		if !ok {
			return nil, fmt.Errorf("asynq: task %q sets consume: true but no tasks.New task with that name is defined in this process; a tasks.Method task is consumed by selecting the plugin that provides it", name)
		}
		out = append(out, taskBinding{binding: binding, owner: plugin.Identity{Plugin: Key}})
	}
	return out, nil
}

// checkTaskQueues refuses a task this process consumes whose configured queue
// is not fetched by the worker that runs it. Queue, retries, and timeout come
// from the deployment-wide configuration, but consumption is per process: a
// worker that does not fetch the task's queue would never see its
// submissions, which is a startup error rather than a task that mysteriously
// waits in Redis forever. Processes that only enqueue are not checked --
// enqueueing a workload's task from a process that does not host the workload
// is the composition the split exists for.
func checkTaskQueues(cfg Config, bindings []taskBinding, groups []*workerGroup) error {
	for _, group := range groups {
		for _, task := range bindings {
			if task.workload != group.workload {
				continue
			}
			name := task.binding.Name()
			queue := cfg.DefaultQueue
			if declared, ok := cfg.Tasks[name]; ok && declared.Queue != "" {
				queue = declared.Queue
			}
			if _, consumed := group.queues[queue]; !consumed {
				return fmt.Errorf("asynq: task %q is consumed by %s but is enqueued to queue %q, which that worker does not consume", name, groupName(group.workload), queue)
			}
		}
	}
	return nil
}

// taskLookup indexes the consumed bindings by name for the tasks contract's
// Run. The same task name may be bound in more than one group -- the queue
// decides which handler a submitted task reaches -- but Run does not go
// through a queue, so it resolves the first collected binding; the collection
// order makes that deterministic.
func taskLookup(bindings []taskBinding) map[string]tasks.Binding {
	lookup := make(map[string]tasks.Binding, len(bindings))
	for _, task := range bindings {
		name := task.binding.Name()
		if _, exists := lookup[name]; !exists {
			lookup[name] = task.binding
		}
	}
	return lookup
}

// resolvedTaskOptions maps every configured task name to the enqueue options
// that carry its deployment settings. A name with no entry carries none and
// takes the client's own defaults unchanged, and a zero Queue or Timeout in an
// entry takes the top-level default the same way. MaxRetries is the exception:
// its zero is a value, not a default, so a configured zero disables retries.
func resolvedTaskOptions(cfg Config) map[string][]TaskOption {
	if len(cfg.Tasks) == 0 {
		return nil
	}
	options := make(map[string][]TaskOption, len(cfg.Tasks))
	for name, task := range cfg.Tasks {
		var forTask []TaskOption
		if task.Queue != "" {
			forTask = append(forTask, Queue(task.Queue))
		}
		if task.MaxRetries != nil {
			forTask = append(forTask, MaxRetries(*task.MaxRetries))
		}
		if task.Timeout > 0 {
			forTask = append(forTask, Timeout(task.Timeout))
		}
		if forTask != nil {
			options[name] = forTask
		}
	}
	return options
}

// installRemoteExecutor registers executor as the process-wide remote task
// executor and returns the function that unregisters it. Exactly one remote
// executor is bound per process: when another one already holds the slot (a
// second asynq instance, or a second App in the same process), the existing
// binding is kept, the installation is reported as not installed, and a
// warning is logged instead of failing construction.
func installRemoteExecutor(executor tasks.Remote, logger log.Logger) func() {
	uninstall, installed := tasks.InstallRemote(executor)
	if installed {
		return uninstall
	}
	if logger == nil {
		logger = log.L()
	}
	logger.Warn("asynq: a remote task executor is already installed; keeping the existing binding")
	return func() {}
}

// remoteExecutor adapts the Plugin to the tasks.Remote SPI: submissions are
// enqueued through the same client the Enqueuer contract exposes, each
// carrying the options its task name resolves to, and Run falls back to the
// bindings this process consumes when no local executor is installed.
type remoteExecutor struct {
	client   *Client
	options  map[string][]TaskOption
	bindings map[string]tasks.Binding
}

var _ tasks.Remote = (*remoteExecutor)(nil)

// Submit implements tasks.Remote. A client closed by shutdown reports
// tasks.ErrClosed while keeping asynq.ErrClosed reachable; every other failure
// -- Redis unreachable, a cancelled submission context -- surfaces as the
// client returned it.
func (e *remoteExecutor) Submit(ctx context.Context, name string, payload []byte) error {
	_, err := e.client.Enqueue(ctx, Task{Type: name, Payload: payload}, e.options[name]...)
	if errors.Is(err, ErrClosed) {
		return fmt.Errorf("%w: %w", tasks.ErrClosed, err)
	}
	return err
}

// Lookup implements tasks.Remote: Run resolves a method task's binding here
// when the local executor is absent, so a process that only consumes runs the
// providing plugin's method in the caller's goroutine without going through
// Redis.
func (e *remoteExecutor) Lookup(name string) (tasks.Binding, bool) {
	binding, ok := e.bindings[name]
	return binding, ok
}
