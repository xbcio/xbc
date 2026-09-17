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
		a.reportShutdown(reason, budget)
	})
	return a.unwindErr
}

// reportShutdown makes the budget's casualties visible to an operator. A clean
// reverse unwind stays silent at warn level; anything the budget cut short
// names the exact plugins, because "the process exited" alone hides skipped
// cleanup. Either way the per-instance waits are recorded, because the
// casualty list names who was cut off and not who spent the budget.
func (a *App) reportShutdown(reason string, budget time.Duration) {
	abandoned := a.shutdownReport.Identities(assembly.StopAbandoned)
	notAttempted := a.shutdownReport.Identities(assembly.StopNotAttempted)
	if len(abandoned) == 0 && len(notAttempted) == 0 {
		a.reportShutdownTimings(reason, budget)
		return
	}
	a.log().Warn("xbc: shutdown budget expired before the reverse unwind finished",
		"reason", reason,
		"budget", budget.String(),
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
func (a *App) reportShutdownTimings(reason string, budget time.Duration) {
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
		"waited", waited,
	)
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
