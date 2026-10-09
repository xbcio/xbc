// Command workloads demonstrates one binary that assembles different subsets of
// itself, decided by placement rather than by build tags, separate mains, or a
// role flag.
//
// The application declares three groups of plugins:
//
//   - transcode, an exclusive workload: a process carrying it carries nothing
//     else.
//   - ingest, an ordinary co-resident workload: a process carrying it may carry
//     any other non-exclusive workload.
//   - heartbeat, an unowned plugin: it belongs to no workload, so every process
//     shape carries it.
//   - asynq, the queue integration: a shared plugin, not a workload. Its worker
//     for a workload exists only in the processes that carry that workload, so
//     the ingest group above brings its queue consumer along with its timer.
//     This file leaves it unconfigured -- see application.yml -- which is what
//     starts the example without a Redis. Where it is configured, a process
//     carrying ingest consumes ingest's queue and one that carries transcode
//     does not.
//
// Which of the two workloads this process carries is decided by
// workloads.<key>.enabled under the default StaticPlacement. Nothing below
// reads an environment variable, a flag, or an argument to choose a role: the
// composition root always selects every Bundle, and placement decides what is
// actually built. See application.yml for the three roles and the commands that
// start each.
//
// Run from the repository root with:
//
//	go run ./examples/workloads --config examples/workloads/application.yml
//	curl localhost:8080/api/v1/heartbeat
//	curl localhost:8080/api/v1/ingest/status
//
// Watch the role change without editing Go. This starts the exclusive role
// instead, in which the ingest route does not exist at all:
//
//	XBC_WORKLOADS_TRANSCODE_ENABLED=true \
//	XBC_WORKLOADS_INGEST_ENABLED=false \
//	  go run ./examples/workloads --config examples/workloads/application.yml
//
// Use the doctor subcommand to see the decision without constructing anything,
// or the validate subcommand to see what the role it decided would actually
// serve. Doctor prints the placement source, each declared workload's row, and
// the definitions belonging to none of them; validate constructs only the
// definitions that survived the decision, so its route table is the role's own:
//
//	go run ./examples/workloads doctor --config examples/workloads/application.yml
//	go run ./examples/workloads validate --config examples/workloads/application.yml
package main

import (
	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/examples/workloads/internal/heartbeat"
	"github.com/xbcio/xbc/examples/workloads/internal/ingest"
	"github.com/xbcio/xbc/examples/workloads/internal/transcode"
	"github.com/xbcio/xbc/extensions/jobs/asynq"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/prelude"
)

func main() {
	xbc.Run(
		// StaticPlacement is the default, and it is written out here because
		// placement is what this example is about: it is the decision point
		// between "this process carries the workload" and "it does not".
		// Omitting the option is exactly equivalent. A deployment that claims
		// roles by lease passes a source that reads the lease instead, and
		// nothing below this line changes.
		xbc.WithPlacement(xbc.StaticPlacement()),
		xbc.WithBundles(
			// The transport and the unowned application plugin: outside the
			// placement question, so every role carries them.
			prelude.Bundle(),
			ginengine.Bundle(),
			heartbeat.Bundle(),
			// Both workloads are always selected. Selecting a Bundle makes its
			// Definitions available; placement decides whether they are built.
			// A workload this process does not carry contributes no instance,
			// no configuration binding, and no route.
			transcode.Bundle(),
			ingest.Bundle(),
			// The queue integration is selected here as unconditionally as
			// everything else. It is a WhenConfigured plugin: without a
			// plugins.asynq section it does not enter the plan at all, so this
			// example still starts with nothing but its own config file and no
			// Redis. Where it is configured, it collects the handler
			// contributions of whichever workloads this process carries -- so
			// the worker consuming ingest's queue is placed by placement, not
			// by this line.
			asynq.Bundle(),
		),
	)
}
