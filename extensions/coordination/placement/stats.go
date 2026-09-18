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
type Held struct {
	// Workload is the slot's workload. It is the only label the metrics use,
	// because a slot index is a placement detail rather than a dimension a
	// dashboard should aggregate over.
	Workload plugin.WorkloadKey
	// Slot is the index within the workload's replica set, and Key is the full
	// lease key. Both are diagnostics.
	Slot int
	Key  string
	// Owner is the token the store associates with this claim.
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
	// Standby reports that this process won no slot and is hosting only the
	// plugins that belong to no workload.
	Standby bool
	// Held is every slot this process holds, ordered by workload then slot.
	Held []Held
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
	held := append([]*heldSlot(nil), p.held...)
	p.mu.Unlock()

	stats := Stats{
		Source:        decision.Source,
		Standby:       resolved && len(held) == 0,
		RenewFailures: p.renewFailures.Load(),
	}
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
