package plugin

import "context"

// Placement records the resolved hosting decision for one boot: which declared
// workloads this process carries, and who decided. It is the answer to a
// question that must be settled before the dependency graph is built, because
// an unhosted workload's Definitions never enter the plan at all.
//
// It describes one process, not the cluster. Nothing here enumerates the other
// processes carrying the same workload, and no member reports cluster
// membership: each process states what it holds, and aggregating those
// statements is the monitoring side's job.
type Placement struct {
	// Source names the PlacementSource that produced this decision, for
	// diagnostics only ("static", "lease", or a source's own label).
	Source string
	// Holder identifies the claimant, and is empty when nothing was claimed --
	// a static decision claims nothing, and neither does a process that won no
	// slot.
	//
	// A source that was given PlacementRequest.Instance reports that, because
	// the identity an operator can act on is the process, not the token some
	// store happens to associate with the claim. A source given no instance
	// reports whatever it can name instead, which for a lease source is the
	// owner token. Diagnostics only: nothing derives behaviour from it.
	Holder string
	// Hosted lists the workloads this process carries. The runtime keeps the
	// order sorted by key when it accepts a decision, so a source may answer in
	// whatever order it discovered the set without a diagnostic depending on it.
	Hosted []WorkloadKey
	// Notes are free-form, one per line, diagnostic only -- an operator-facing
	// explanation a source may attach (why a workload was declined, which slot
	// index was claimed, and so on). Never interpreted.
	Notes []string
}

// PlacementRequest is everything a PlacementSource may consult. It carries no
// configuration values and no constructed resources: a source decides before
// the graph exists, so it can read nothing else.
type PlacementRequest struct {
	// Workloads are the workloads the composition declares, sorted by key.
	Workloads []Workload
	// Instance is this process's identity -- xbc.instance_id, derived when the
	// deployment did not set one.
	//
	// It is here because a source that claims something on this process's
	// behalf is the only party that can record which process claimed it, and it
	// cannot work that out for itself: the identity is settled while
	// configuration binds, and a source is built by the composition root before
	// that happens. Without it a claim is attributable only to whatever token
	// the underlying store invented, which names nothing an operator can go and
	// look at.
	//
	// Empty means the caller had no identity to offer, which is what a direct
	// caller outside a run has. A source must still work, and reports whatever
	// it can name as Placement.Holder instead.
	Instance string
	// Enabled reports whether configuration admits a workload at all. A
	// workload with Enabled false is a hard veto no source may override: a
	// deployment uses it to exclude a process from a role outright.
	//
	// nil means every declared workload is admitted, which is what a caller
	// with no configuration to consult asks for.
	Enabled func(WorkloadKey) bool
}

// Admits reports whether configuration admits key. A nil Enabled admits
// everything, so a source never has to spell that case out itself.
func (r PlacementRequest) Admits(key WorkloadKey) bool {
	if r.Enabled == nil {
		return true
	}
	return r.Enabled(key)
}

// PlacementSource decides which declared workloads this process hosts.
//
// Resolve runs once, before the assembly plan is built, and must be
// deterministic for a given request. An error fails startup: a process whose
// role cannot be decided must not guess, because every downstream decision --
// doctor output, startup validation, exclusivity, snapshot diffing -- is
// derived from the hosted set.
//
// A source that has to acquire something to answer -- a lease slot, a lock, a
// reservation -- also implements PlacementReleaser, so the runtime can give
// that claim back on every path that does not reach the plugin which owns it.
type PlacementSource interface {
	Resolve(PlacementRequest) (Placement, error)
}

// PlacementReleaser is the optional second half of a PlacementSource: it gives
// back whatever the source acquired while resolving.
//
// The runtime consults a source before it plans, and on a running application
// the constructed plugin graph owns what the decision acquired -- the lease
// source's slots are released by its own PreStop and Stop hooks. The runtime
// calls Release when the run ends regardless, as the backstop behind those
// hooks: doctor, a plan that fails to build, a plan that enables nothing, a
// stop during planning and a composition that never selected the placement
// plugin all reach it with no hook that could have released anything, and a
// stop the shutdown budget abandoned reaches it with a Stop that never ran. It
// is called even when Resolve failed part-way or was never reached, so a
// release with nothing to give back must be a no-op.
//
// Release must give back only what this process acquired, must be idempotent --
// the hooks above may have released the same claim already, and the runtime
// calls it on every completed run -- and must work on a run that is already
// stopping, because those are the paths that most need it. The runtime supplies
// a context that does not inherit the run's cancellation and bounds the call
// with a short deadline of its own, so a claim the store does not confirm
// inside that budget is left to expire with its lease rather than holding the
// command open.
type PlacementReleaser interface {
	Release(context.Context) error
}
