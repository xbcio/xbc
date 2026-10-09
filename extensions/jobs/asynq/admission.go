package asynq

import (
	"context"
	"fmt"

	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

// admittedHandler charges one unit of a workload's admission quota for the
// duration of every handler invocation.
//
// It sits between the tracker and the dispatcher, so what it covers is exactly
// the handler running: the task is already dequeued by the time the library
// calls this, and the unit is held until the handler returns. That is the
// quantity the workload's max_goroutines bounds but the managed-task path
// cannot see -- the framework never started these goroutines -- so without it
// "queue concurrency" is the only bound on how much of a workload runs at once,
// and a process hosting several workloads cannot tell whose work is filling its
// CPU.
//
// The wait is blocking and bounded by the task's own context: it ends at the
// task's deadline and, during shutdown, when the library cancels the handler's
// context after ShutdownTimeout passed without the quota freeing up. Refusing
// instead would mean returning an error for a task this worker already
// accepted, which burns a retry attempt and eventually archives the work --
// and the task is not bad, it is only early: the quota is taken by this
// process's own work, which is work that finishes.
type admittedHandler struct {
	admission plugin.Admission
	inner     hibiken.Handler
}

// ProcessTask implements hibiken.Handler.
func (h admittedHandler) ProcessTask(ctx context.Context, task *hibiken.Task) error {
	release, err := h.admission.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("asynq: task %q waited for its workload's admission and could not take it: %w", task.Type(), err)
	}
	defer release()
	return h.inner.ProcessTask(ctx, task)
}
