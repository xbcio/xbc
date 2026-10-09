package ingest

import (
	"context"

	"github.com/xbcio/xbc/extensions/jobs/asynq"
)

// BatchTaskType is the task the ingest workload's worker handles. The constant
// is exported so code that enqueues a batch names the same string this package
// registers, rather than a literal two sides keep in step by hand.
const BatchTaskType = "ingest.batch"

var _ asynq.HandlerContributor = (*Plugin)(nil)

// TaskHandlers implements asynq.HandlerContributor.
//
// This is the queue-driven half of the workload the ticker in ingest.go drives,
// and it runs the same unit of work: one delivery admits one batch. What makes
// it a member of the workload rather than a process-wide worker is the Bundle
// this Definition is tagged with. Asynq groups its worker servers by the
// workload their contributors belong to, so:
//
//   - a worker for these handlers exists only in a process that carries the
//     ingest workload. The exclusive and standby roles select the same Bundles
//     from the same main.go, and run no worker for ingest.batch at all -- which
//     is what lets a deployment put a workload's queue consumers in exactly the
//     processes that carry it, instead of every process that happens to
//     configure the queue integration.
//   - a delivery is admitted against this workload's
//     workloads.ingest.max_goroutines budget, drawing on the same counter the
//     ticker's managed task draws on. Queue work cannot outgrow the limit the
//     workload declared, and a delivery that finds the budget taken waits for a
//     unit rather than failing the task.
//
// The queue the worker consumes is a deployment decision this package cannot
// make, so it is declared beside the workload:
// plugins.asynq.workloads.ingest.queues in application.yml. A process that
// carries this workload and configures the queue integration must name that
// set: which queue a workload's tasks arrive on is not derivable from the task
// types its handlers register.
func (p *Plugin) TaskHandlers() []asynq.HandlerRegistration {
	return []asynq.HandlerRegistration{{
		Type: BatchTaskType,
		Handler: asynq.HandlerFunc(func(context.Context, asynq.Task) error {
			p.batch()
			return nil
		}),
	}}
}
