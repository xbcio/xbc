package placement

import (
	"context"
	"fmt"
	"strings"

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

// renewalHealth reports the first held slot whose most recent renewal was not
// confirmed, and is nil when every held slot is confirmed or none is held.
//
// It stays down until a later renewal succeeds, which is the honest answer: the
// process is still doing the work, and an operator is being told that its claim
// on the capacity is not, so a takeover elsewhere may already be under way.
func (p *Placement) renewalHealth() error {
	var degraded []string
	for _, slot := range p.snapshotHeld() {
		state := slot.snapshot()
		if state.degraded {
			degraded = append(degraded, fmt.Sprintf("%s slot %d", state.workload, state.index))
		}
	}
	if len(degraded) == 0 {
		return nil
	}
	return fmt.Errorf("placement: lease renewal is not being confirmed for %s", strings.Join(degraded, ", "))
}
