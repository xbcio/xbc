package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func (a *App) abort(cause error) error {
	if err := a.unwind(stopReasonStartupFailed); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// unwind is idempotent and applies one shared budget to the complete reverse
// walk. Constructed.Unwind invokes Stop before afterStop cancels and joins the
// same Plugin's task scope, and starts no further Stop once the budget is
// spent. drainRemaining then reclaims every task scope the walk did not reach,
// so an abandoned or not-attempted Plugin still cannot leak its goroutines.
//
// This is also where the pre-stop phase runs, and it runs here rather than in
// wait for a structural reason: "after requestStop, before the reverse walk"
// is not a seam the code has. unwind opens with requestStop, abort and the
// migrate-only subcommand both reach unwind without ever passing through wait,
// and all three must agree on whether the phase runs. Putting it inside the
// same unwindOnce that already makes the reverse walk idempotent gives the
// phase its once-guard for free and leaves exactly one place that decides.
func (a *App) unwind(reason string) error {
	a.requestStop(reason)
	// Whatever startup was waiting for, it is no longer waiting for it: from
	// here the process is cleaning up under the shutdown budget, which reports
	// itself. See startupProgress.conclude for why the slow-startup report has
	// to fall silent here specifically and not on the stop request.
	a.progress.conclude()
	a.unwindOnce.Do(func() {
		if a.tasks != nil {
			a.tasks.closeAdmission()
		}
		// The pre-stop phase precedes the reverse walk because its whole
		// purpose is to retract a value's external participation while the
		// process is still fully alive -- in particular while its managed
		// tasks still run, since a task scope is only cancelled once its
		// plugin's Stop has returned. Running it after any Stop would make it
		// mean nothing.
		phase := a.runPreStopPhase()
		a.reportPreStop(phase)
		budget := a.settings.ShutdownTimeout
		if budget <= 0 {
			budget = 30 * time.Second
		}
		deadline, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()

		var errs []error
		if a.owned != nil {
			report, err := a.owned.Unwind(deadline, budget, func(identity plugin.Identity) error {
				if a.tasks == nil {
					return nil
				}
				return a.tasks.stopPlugin(identity, deadline)
			})
			a.shutdownReport = report
			if err != nil {
				errs = append(errs, err)
			}
		}
		if a.tasks != nil {
			if err := a.tasks.drainRemaining(deadline); err != nil {
				errs = append(errs, err)
			}
		}
		a.unwindErr = errors.Join(errs...)
		a.reportShutdown(reason, budget, phase)
	})
	return a.unwindErr
}

// preStopPhase is what one execution of the pre-stop phase produced. It is a
// value rather than a set of App fields because the phase runs exactly once
// inside the single unwindOnce that also produces the shutdown report, so
// nothing outside that closure ever has to read it back.
type preStopPhase struct {
	// ran is false when the phase was skipped: a budget of 0s, nothing owned
	// yet, or a startup that never reached the running state. A skipped phase
	// reports nothing at all, which is what keeps an application that declares
	// no PreStop anywhere byte-for-byte identical to one built before the
	// stage existed.
	ran     bool
	budget  time.Duration
	elapsed time.Duration
	// report carries the per-hook outcomes, including which hook failed and
	// with what error. It is the only record the reporting needs: an earlier
	// version also stored the joined error next to it, but nothing read it,
	// and leaving an unread field here invites a future reader to report the
	// phase twice from two sources.
	report assembly.PreStopReport
}

// runPreStopPhase executes the pre-stop phase for an application that reached
// the running state, and does nothing at all for one that did not.
//
// The gate is the traffic gate, which releaseTraffic opens exactly once and
// only on the path that finishes startup. That is the same condition as "every
// Start hook returned", but stated in the one place the runtime already
// records it, and it gets the late case right as well: an application that
// failed liveness after the gate opened has already run every Start hook, so
// its plugins may genuinely hold something and the phase must still run. An
// application that failed before the gate -- construction, migration, Start,
// traffic preparation -- holds nothing, so running the phase would ask plugins
// to retract a participation they never established.
//
// The budget is whole-phase, exactly as ShutdownTimeout is whole-shutdown, and
// for the same reason: Constructed.PreStop hands every hook the same deadline,
// so a per-plugin budget would make the worst case scale with the number of
// plugins instead of staying bounded.
func (a *App) runPreStopPhase() preStopPhase {
	if !a.trafficReleased() || a.owned == nil {
		return preStopPhase{}
	}
	budget := a.settings.PreStopTimeout
	if budget <= 0 {
		return preStopPhase{}
	}
	// The context is derived from Background, never from the execution
	// context and never from a plugin.Context. requestStop has already
	// cancelled the execution context by the time this runs, so a hook wired
	// to it would fail on its first remote call with context.Canceled -- and
	// because a failed PreStop does not fail the run, that would turn every
	// release into a failure the process exits cleanly through anyway.
	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	started := time.Now()
	// The joined error is deliberately dropped rather than recorded: the phase
	// never fails the run, and every individual failure it summarizes is
	// already in the report, attributed to the hook that produced it. Keeping
	// the joined form as well would give a later reader a second, less precise
	// source for the same fact.
	report, _ := a.owned.PreStop(deadline, budget)
	return preStopPhase{
		ran:     true,
		budget:  budget,
		elapsed: time.Since(started),
		report:  report,
	}
}

// trafficReleased reports whether startup reached the point where ingress was
// opened. It is read under the same mutex releaseTraffic writes it under, so a
// concurrent stop request cannot make it a torn read; the value only ever goes
// from false to true, and only on the goroutine that drives execute.
func (a *App) trafficReleased() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.trafficOpen
}

// reportPreStop makes the phase visible to an operator, and stays silent when
// there is nothing to say.
//
// The asymmetry matches reportShutdown's: a plugin that ignored its budget is a
// contract violation the operator has to fix, so it warns and names the
// plugins; a clean phase is only interesting next to the shutdown that follows,
// so it is reported at debug with its cost. A skipped phase says nothing at all
// -- an application that declares no PreStop anywhere is the ordinary case, and
// a line about it on every boot would train operators to ignore the one boot
// where it matters.
//
// A hook that returned an error, or panicked, warns for a different reason than
// the budget does. Unlike Stop, a failed PreStop does not fail the run: the
// phase exists to hand work over voluntarily, and refusing to exit because a
// retraction failed would turn a degraded store into an outage. That makes the
// log the only place the failure can appear, and without this line it appears
// nowhere at all -- the process exits 0 and the release that did not happen is
// indistinguishable from one that did. Silence here would defeat the entire
// reason PreStop gets a live context rather than a cancelled one.
func (a *App) reportPreStop(phase preStopPhase) {
	if !phase.ran {
		return
	}
	abandoned := phase.report.Identities(assembly.PreStopAbandoned)
	if len(abandoned) > 0 {
		a.log().Warn("xbc: pre-stop budget expired before every hook returned",
			"budget", phase.budget.String(),
			"abandoned", identityLabels(abandoned),
			"waited", phase.report.Waited(),
		)
	}
	failed := phase.report.Identities(assembly.PreStopFailed)
	panicked := phase.report.Identities(assembly.PreStopPanicked)
	if len(failed) > 0 || len(panicked) > 0 {
		a.log().Warn("xbc: pre-stop hooks did not finish cleanly",
			"budget", phase.budget.String(),
			"elapsed", phase.elapsed.String(),
			"failed", identityLabels(failed),
			"panicked", identityLabels(panicked),
			"errors", preStopErrorLabels(phase.report),
		)
	}
	if len(abandoned) > 0 || len(failed) > 0 || len(panicked) > 0 {
		return
	}
	if !a.log().Enabled(log.DebugLevel) {
		return
	}
	a.log().Debug("xbc: pre-stop phase finished inside its budget",
		"budget", phase.budget.String(),
		"elapsed", phase.elapsed.String(),
		"waited", phase.report.Waited(),
	)
}

// preStopErrorLabels renders the hooks that reported an error, so the warning
// names what actually failed rather than only which plugin failed. A panic is
// included because it is recovered and turned into an error, and losing that
// text would leave an operator with a plugin name and no cause.
func preStopErrorLabels(report assembly.PreStopReport) []string {
	var labels []string
	for _, record := range report.Records {
		if record.Err == nil {
			continue
		}
		labels = append(labels, record.Identity.String()+": "+record.Err.Error())
	}
	return labels
}

// reportShutdown makes the budget's casualties visible to an operator. A clean
// reverse unwind stays silent at warn level; anything the budget cut short
// names the exact plugins, because "the process exited" alone hides skipped
// cleanup. Either way the per-instance waits are recorded, because the
// casualty list names who was cut off and not who spent the budget.
//
// phase is carried in so that both lines can state the stop's real ceiling.
// One stop now costs pre_stop_timeout plus shutdown_timeout, and a supervisor
// that kills at the old single budget would cut the process off in the phase
// that exists precisely to let it hand its work over.
func (a *App) reportShutdown(reason string, budget time.Duration, phase preStopPhase) {
	abandoned := a.shutdownReport.Identities(assembly.StopAbandoned)
	notAttempted := a.shutdownReport.Identities(assembly.StopNotAttempted)
	total := budget
	if phase.ran {
		total += phase.budget
	}
	if len(abandoned) == 0 && len(notAttempted) == 0 {
		a.reportShutdownTimings(reason, budget, phase)
		return
	}
	a.log().Warn("xbc: shutdown budget expired before the reverse unwind finished",
		"reason", reason,
		"budget", budget.String(),
		"pre_stop_budget", preStopBudgetLabel(phase),
		"total_budget", total.String(),
		"abandoned", identityLabels(abandoned),
		"not_attempted", identityLabels(notAttempted),
		"waited", stopWaitLabels(a.shutdownReport),
	)
}

// reportShutdownTimings answers the question the warning cannot: a shutdown
// that stayed inside its budget can still dominate a rolling restart, and
// nothing else says which plugin's Stop the seconds went to. It is debug for
// the same reason the startup breakdown is: its size follows the number of
// selected plugins.
//
// The pre-stop phase is stated beside the walk because the two are one stop to
// a supervisor. Its own line already reported what each hook cost; what this
// one adds is the sum an operator sizes a kill grace period against.
func (a *App) reportShutdownTimings(reason string, budget time.Duration, phase preStopPhase) {
	if !a.log().Enabled(log.DebugLevel) {
		return
	}
	waited := stopWaitLabels(a.shutdownReport)
	if len(waited) == 0 {
		return
	}
	a.log().Debug("xbc: reverse unwind finished inside its budget",
		"reason", reason,
		"budget", budget.String(),
		"pre_stop", preStopElapsedLabel(phase),
		"total_budget", preStopTotalLabel(phase, budget),
		"waited", waited,
	)
}

// preStopBudgetLabel renders the pre-stop budget for the reverse walk's
// report, so that the line states the stop's whole ceiling rather than the
// half of it this walk controls.
func preStopBudgetLabel(phase preStopPhase) string {
	if !phase.ran {
		return "skipped"
	}
	return phase.budget.String()
}

// preStopElapsedLabel renders what the phase actually cost, which is what
// tells an operator whether the budget it was given is the right size.
func preStopElapsedLabel(phase preStopPhase) string {
	if !phase.ran {
		return "skipped"
	}
	return phase.elapsed.String()
}

// preStopTotalLabel renders the two budgets one stop is bounded by, as their
// sum. It is a sum and not an observation: the point is the ceiling a
// supervisor has to allow for, and a phase that finished early does not
// lower it.
func preStopTotalLabel(phase preStopPhase, budget time.Duration) string {
	if !phase.ran {
		return budget.String()
	}
	return (budget + phase.budget).String()
}

// stopWaitLabels renders how long the walk waited on each instance it actually
// waited for, in reverse graph order. Skipped and not-attempted instances are
// omitted rather than reported as "0s": nothing was waited for there, and a
// zero would read as a Stop that returned instantly. The outcome decides that,
// not the measured duration, so a genuinely instant Stop is still reported.
func stopWaitLabels(report assembly.ShutdownReport) []string {
	var labels []string
	for _, record := range report.Records {
		if record.Outcome == assembly.StopSkipped || record.Outcome == assembly.StopNotAttempted {
			continue
		}
		labels = append(labels, record.Identity.String()+" "+record.Duration.String())
	}
	return labels
}

func identityLabels(identities []plugin.Identity) []string {
	labels := make([]string, len(identities))
	for index, identity := range identities {
		labels[index] = identity.String()
	}
	return labels
}
