package plugin

import "context"

// Admission is one workload's admission quota: the bound on how many units of
// that workload's work may be in flight at once.
//
// It exists because a managed task is not the only way a workload runs work.
// Context.Go and Context.GoCritical are the framework's own submission path,
// and workloads.<key>.max_goroutines charges every task that goes through them.
// Work a plugin runs on goroutines it did not submit -- a queue worker handling
// the tasks hibiken's own pool dequeued, a task handed to a pool executor --
// never passes through them, so nothing counts it, and "queue concurrency
// times pool width" is exactly the product no knob bounds. A plugin that runs
// such work reports each unit to this limiter instead.
//
// The quota is one number per workload, shared by both kinds of work: managed
// tasks and admitted units draw on the same count, so max_goroutines bounds the
// workload's concurrent units of work however they are started. There is
// deliberately no second budget.
//
// A Context never reports a nil Admission. A plugin that belongs to no
// workload, or whose workload declares no max_goroutines, gets a limiter that
// admits immediately and whose release does nothing, so a caller never has to
// branch on whether it is bounded.
type Admission interface {
	// Acquire takes one unit of the quota and returns the release that gives it
	// back. It blocks while the workload is at its limit, and reports the
	// context's error if that wait ends first -- in which case release is nil
	// and nothing was taken. Release is idempotent.
	//
	// Blocking is the whole point: a queue has no client to refuse, and a
	// refusal there means a retry, a burned attempt and eventually an archived
	// task. Waiting holds the goroutine that would otherwise run the work, which
	// is precisely the backpressure the quota is for. Callers pass a context
	// that ends when their work is abandoned (shutdown), so a wait cannot
	// outlive the reason it was waiting.
	Acquire(ctx context.Context) (release func(), err error)
}

// noAdmission is the limiter reported for work that nothing bounds: a plugin
// outside every workload, a workload that declares no budget, or a process
// whose host supplies no limiter. Its Acquire never blocks, which is what lets
// every caller use the limiter unconditionally.
type noAdmission struct{}

// Acquire implements Admission by granting immediately.
func (noAdmission) Acquire(context.Context) (func(), error) { return func() {}, nil }
