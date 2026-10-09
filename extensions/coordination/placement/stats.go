package placement

import (
	"sort"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// Held is one slot this process holds, as reported by Stats.
//
// Its field names are the labels and values a metrics bridge exports:
//
//	xbc_workload_held{workload="sast"} 1
//	xbc_workload_lease_age_seconds{workload="sast"} <Age>
//
// Stats.Instance belongs beside those as a second label, but it is a property
// of the process rather than of a slot, so it is reported once there instead of
// being repeated on every row.
type Held struct {
	// Workload is the slot's workload. It is the only per-slot label the
	// metrics use, because a slot index is a placement detail rather than a
	// dimension a dashboard should aggregate over.
	Workload plugin.WorkloadKey
	// Slot is the index within the workload's replica set, and Key is the full
	// lease key. Both are diagnostics.
	Slot int
	Key  string
	// Owner is the token the store associates with this claim. It is what a
	// slot key's value holds, so it is the field that turns a claim found by
	// reading the store into this process's claim.
	//
	// It is not the bare process identity: the backend composes Stats.Instance
	// with a part unique to the one acquisition, so the value read out of the
	// store names the holder while remaining safe to compare against. Instance
	// is the field to report to a person; this is the field to compare with a
	// store.
	Owner string
	// Age is how long the slot has been held, and Renewed is when it was last
	// confirmed.
	Age     time.Duration
	Renewed time.Time
	// Degraded reports that the most recent renewal was not confirmed. The
	// process keeps hosting a degraded slot by design, so this is the field
	// that tells an operator "still working, but the claim is not being
	// confirmed" rather than "stopped".
	Degraded bool
}

// Declared is one workload this process's declaration admits, as reported by
// Stats: the capacity the deployment asked for, as opposed to the slots this
// process happened to win.
//
// It exists so a monitor does not have to copy Go constants into alert rules.
// "How many replicas of sast are running" is an aggregation over per-process
// metrics, and "fewer than declared" or "declared but held by nobody" are the
// two questions that aggregation is asked; both need the declared number next
// to the held gauge, and only the process can report the number it was
// assembled with.
type Declared struct {
	// Workload is the declared workload's key, the same label the held gauge
	// carries.
	Workload plugin.WorkloadKey
	// Replicas is how many processes the declaration allows to hold this
	// workload at once, and Exclusive reports that a holder may hold nothing
	// else. Both are cluster-level constraints read from this process's own
	// declaration, so every process that admits the workload reports the same
	// values for it.
	Replicas  int
	Exclusive bool
}

// Stats is one lock-free-enough snapshot of what this process's placement is
// doing.
//
// It exists because core owns no metrics registry: the Prometheus registry is a
// Web extension, and this module deliberately does not depend on a transport.
// The snapshot is therefore the export point, and the three metric names above
// are what a metrics bridge built on it should publish.
type Stats struct {
	// Source names the placement source, and is "lease" for this package.
	Source string
	// Instance is the process identity the resolving request carried, and is
	// the label a metrics bridge joins these slots to a process by. It is empty
	// when the caller offered none.
	Instance string
	// Standby reports that this process won no slot and is hosting only the
	// plugins that belong to no workload.
	//
	// A process that won slots and then gave them back -- Release, which the
	// runtime calls at the end of every run, behind PreStop and Stop wherever
	// the placement plugin was constructed -- is not reported as one. It took
	// the role for as long as its decision held, and Standby describes the
	// decision rather than the current held set.
	Standby bool
	// Held is every slot this process holds, ordered by workload then slot.
	Held []Held
	// Declared is every workload this process's declaration admits, ordered by
	// key, with the replica count and exclusivity declared for it.
	//
	// It is empty until the placement has resolved, and it holds what this
	// process admits rather than the whole cluster's declaration: a workload
	// vetoed here by `workloads.<key>.enabled: false` is absent, because this
	// process may never host it and counting its replicas against a fleet that
	// excluded it would count capacity nobody declared.
	Declared []Declared
	// RenewFailures counts renewals the store did not confirm, which is
	// xbc_workload_lease_renew_failures_total. It is cumulative: a process that
	// recovered from a store outage still shows that the outage happened.
	RenewFailures uint64
}

// Stats returns a snapshot of this placement's own state.
//
// It reports only what this process holds. There is deliberately no way to ask
// how many replicas of a workload are running: answering that needs the lease
// store to support enumeration, which is exactly the promise the contract
// declines to make, and "how many are up" is an aggregation over per-process
// metrics rather than a question any one process can answer.
func (p *Placement) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	decision := p.decision
	resolved := p.resolved
	released := p.released
	instance := p.instance
	held := append([]*heldSlot(nil), p.held...)
	admitted := append([]plugin.Workload(nil), p.admitted...)
	p.mu.Unlock()

	stats := Stats{
		Source:        decision.Source,
		Instance:      instance,
		Standby:       resolved && !released && len(held) == 0,
		RenewFailures: p.renewFailures.Load(),
	}
	for _, workload := range admitted {
		stats.Declared = append(stats.Declared, Declared{
			Workload:  workload.Key,
			Replicas:  workload.Replicas,
			Exclusive: workload.Exclusive,
		})
	}
	sort.Slice(stats.Declared, func(i, j int) bool {
		return stats.Declared[i].Workload < stats.Declared[j].Workload
	})
	now := time.Now()
	for _, slot := range held {
		state := slot.snapshot()
		stats.Held = append(stats.Held, Held{
			Workload: state.workload,
			Slot:     state.index,
			Key:      state.key,
			Owner:    state.owner,
			Age:      now.Sub(state.won),
			Renewed:  state.renewed,
			Degraded: state.degraded,
		})
	}
	sort.Slice(stats.Held, func(i, j int) bool {
		if stats.Held[i].Workload != stats.Held[j].Workload {
			return stats.Held[i].Workload < stats.Held[j].Workload
		}
		return stats.Held[i].Slot < stats.Held[j].Slot
	})
	return stats
}
