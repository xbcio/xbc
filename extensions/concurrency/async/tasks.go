package async

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/extensions/tasks"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// taskProviders collects the tasks.Provider exports of the collected plugins,
// read once at construction. Collecting them places every providing plugin
// upstream of this Pool in the dependency graph, so a locally executed
// handler's dependencies are still running while it runs -- including when the
// drain phase is skipped or times out and Stop is what cancels the work.
var taskProviders = plugin.Collect[tasks.Provider]()

// installLocalExecutor registers pool as the process-wide local task
// executor and returns the function that unregisters it. Exactly one local
// executor is bound per process: when another one already holds the slot (a
// second async instance, or a second App in the same process), the existing
// binding is kept, the installation is reported as not installed, and a
// warning is logged instead of failing construction.
func installLocalExecutor(pool *Pool, logger log.Logger) func() {
	uninstall, installed := tasks.InstallLocal(localExecutor{pool: pool, bindings: pool.bindings})
	if installed {
		return uninstall
	}
	if logger == nil {
		logger = log.L()
	}
	logger.Warn("async: a local task executor is already installed; keeping the existing binding")
	return func() {}
}

// localExecutor adapts the Pool to the tasks.Local SPI: tasks.Go and local
// task submissions become ordinary Spawns, so admission, queue capacity, the
// submit timeout, workload quota, panic recovery, and the async_task pprof
// label are the Pool's own behaviors unchanged.
type localExecutor struct {
	pool     *Pool
	bindings map[string]tasks.Binding
}

var _ tasks.Local = localExecutor{}

// Go implements tasks.Local by spawning fn as a fire-and-forget task.
func (e localExecutor) Go(ctx context.Context, name string, fn func(context.Context)) error {
	return taskError(e.pool.Spawn(ctx, name, fn))
}

// Submit implements tasks.Local: it looks the handler up first, so a task this
// process cannot execute is refused without occupying pool capacity, then
// spawns an execution that decodes the payload inside the task and logs a
// handler failure the way any other spawned task failure is logged.
func (e localExecutor) Submit(ctx context.Context, name string, payload []byte) error {
	binding, ok := e.bindings[name]
	if !ok {
		return fmt.Errorf("%w: task %q", tasks.ErrNoHandler, name)
	}
	handler := binding.Handler()
	return taskError(e.pool.Spawn(ctx, name, func(taskCtx context.Context) {
		if err := handler(taskCtx, payload); err != nil {
			e.pool.log.Error("async: task failed", "name", name, "error", err.Error())
		}
	}))
}

// Lookup implements tasks.Local: tasks.Run resolves a method task's binding
// here, so it runs the providing plugin's method in the caller's goroutine
// without going through the pool.
func (e localExecutor) Lookup(name string) (tasks.Binding, bool) {
	binding, ok := e.bindings[name]
	return binding, ok
}

// taskError maps the Pool's own errors onto the tasks contract's sentinels
// while keeping the Pool's error reachable, so a caller can match either. A
// drained or stopped Pool is tasks.ErrClosed, an exhausted pool is
// tasks.ErrSaturated, and everything else (a cancelled submission context, a
// rejected executor) passes through unchanged.
func taskError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrShuttingDown):
		return fmt.Errorf("%w: %w", tasks.ErrClosed, err)
	case errors.Is(err, ErrSaturated):
		return fmt.Errorf("%w: %w", tasks.ErrSaturated, err)
	default:
		return err
	}
}

// buildTaskTable builds the Pool's private dispatch table from the collected
// providers and the process's function tasks. It is read-only after
// construction, so the executor's Lookup and Submit read it without locking.
//
// Two plugins providing the same task fail construction, mirroring asynq's
// duplicate-handler rule: which handler a task reaches must not depend on the
// order contributors were collected in. A provider binding cannot collide
// with a function task, because both definition paths claim their name in the
// same process-wide registry.
func buildTaskTable(providers []plugin.Entry[tasks.Provider]) (map[string]tasks.Binding, error) {
	table := make(map[string]tasks.Binding)
	for _, binding := range tasks.Funcs() {
		table[binding.Name()] = binding
	}
	owners := make(map[string]plugin.Identity, len(providers))
	for _, provider := range providers {
		bindings, err := providerBindings(provider)
		if err != nil {
			return nil, err
		}
		for position, binding := range bindings {
			name := binding.Name()
			if name == "" {
				return nil, fmt.Errorf("async: task provider %s returned a binding without a name at index %d", provider.Identity, position)
			}
			if binding.Handler() == nil {
				return nil, fmt.Errorf("async: task provider %s returned a binding without a handler for task %q", provider.Identity, name)
			}
			if previous, duplicate := owners[name]; duplicate {
				return nil, fmt.Errorf("async: duplicate task %q from %s and %s", name, previous, provider.Identity)
			}
			owners[name] = provider.Identity
			table[name] = binding
		}
	}
	return table, nil
}

// providerBindings calls the provider's Tasks method, turning a panic in it
// into a construction error instead of letting it escape construction, the
// way asynq's handler collection does.
func providerBindings(provider plugin.Entry[tasks.Provider]) (bindings []tasks.Binding, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			bindings = nil
			err = fmt.Errorf("async: task provider %s Tasks callback panicked: %v", provider.Identity, recovered)
		}
	}()
	return provider.Value.Tasks(), nil
}
