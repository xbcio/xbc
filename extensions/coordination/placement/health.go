package placement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// healthDefinition exports the readiness probe that answers "is this process's
// slot ownership still being confirmed".
//
// It is a second Definition rather than a method on the primary value because
// the primary value is already reached through the Definition that runs the
// lifecycle, and health.Contributor is a contract the graph binds by type.
// Exporting it from a Definition of its own is the same shape the storage
// integrations use for their probes.
var healthDefinition = plugin.Define(
	HealthKey,
	func(plugin.BuildContext) (*healthProbe, error) {
		placement, err := installed()
		if err != nil {
			return nil, err
		}
		return newHealthProbe(placement), nil
	},
	plugin.Options[*healthProbe]{
		Exports: plugin.Contracts(
			plugin.ExportAs[health.Contributor](func(value *healthProbe) health.Contributor { return value }),
		),
	},
)

var healthBundle = plugin.BundleOf(healthDefinition)

// healthProbe reports this process's placement health.
//
// It reads in-memory state and performs no I/O. A probe that called the lease
// store would turn a diagnostics surface into load on the very component whose
// availability is in question, and would report the store's health rather than
// this process's: the question a readiness probe answers is "can this instance
// keep doing its job", and what this instance knows is whether its own claims
// are still being confirmed.
type healthProbe struct {
	placement *Placement
}

var _ health.Contributor = (*healthProbe)(nil)

func newHealthProbe(placement *Placement) *healthProbe {
	return &healthProbe{placement: placement}
}

// HealthChecks returns one readiness check.
//
// A process holding no slot is ready. That is the standby shape, it is a
// deliberate deployment, and reporting it down would make an orchestrator
// replace the very process whose job is to be available to take over.
func (p *healthProbe) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(func(context.Context) error { return p.placement.renewalHealth() }),
	}}
}

// renewalHealth reports the first held slot whose ownership has gone
// unconfirmed for longer than its ttl, and is nil while every held slot is
// confirmed, only recently unconfirmed, or none is held.
//
// The grace period is the part that matters. A renewal that fails says the
// store did not answer this round, not that the claim is gone: the key keeps
// the ttl it was last written with, so a holder whose store blipped for a
// moment still owns its slot and is still the process doing the work. Reporting
// readiness down on the first failed round would take every holder of a
// workload out of rotation at the same instant -- they share the store, so they
// share the blip -- and hand all of their traffic to the standbys, which is a
// worse answer than the one the blip itself produced. It also contradicts the
// premise the lease design rests on: the store is not required to be highly
// available, so a renewal that cannot reach it must be survivable.
//
// Once a full ttl has passed without confirmation the answer changes, and it is
// deliberately the same quantity the store works in: the key expires a ttl
// after it was last written, so from that moment another process may legitimately
// have won the slot and this one may be a second copy of the workload. That is
// the state readiness exists to report.
//
// The in-between state is not silent -- it is a metric, not a probe. Degraded
// slots stay visible in Stats.Held, and every unconfirmed round is counted in
// Stats.RenewFailures, so an alert can fire on a store that is failing without
// the orchestrator being told to stop routing to processes that are still the
// only ones running their workload.
func (p *Placement) renewalHealth() error {
	now := time.Now()
	var degraded []string
	for _, slot := range p.snapshotHeld() {
		state := slot.snapshot()
		if !state.degraded {
			continue
		}
		unconfirmed := now.Sub(state.renewed)
		if unconfirmed <= p.ttl {
			continue
		}
		degraded = append(degraded, fmt.Sprintf("%s slot %d (unconfirmed for %s)", state.workload, state.index, unconfirmed.Round(time.Second)))
	}
	if len(degraded) == 0 {
		return nil
	}
	return fmt.Errorf("placement: lease renewal is not being confirmed for %s", strings.Join(degraded, ", "))
}
