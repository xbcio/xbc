package plugin

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
	// Holder identifies the claimant: empty for a static decision, the lease
	// owner token for a lease decision.
	Holder string
	// Hosted lists the workloads this process carries, sorted by key.
	Hosted []WorkloadKey
	// Notes are free-form, one per line, diagnostic only -- an operator-facing
	// explanation a source may attach (why a workload was declined, which slot
	// index was claimed, and so on). Never interpreted.
	Notes []string
}

// Hosts reports whether key is in Hosted.
//
// It is read on the hosted-set path rather than by scanning Hosted at each call
// site, so "is this workload carried" has exactly one spelling for the
// assembly layer, the diagnostics and the tests.
func (p Placement) Hosts(key WorkloadKey) bool {
	for _, hosted := range p.Hosted {
		if hosted == key {
			return true
		}
	}
	return false
}

// PlacementRequest is everything a PlacementSource may consult. It carries no
// configuration values and no constructed resources: a source decides before
// the graph exists, so it can read nothing else.
type PlacementRequest struct {
	// Workloads are the workloads the composition declares, sorted by key.
	Workloads []Workload
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
type PlacementSource interface {
	Resolve(PlacementRequest) (Placement, error)
}
