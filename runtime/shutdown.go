package runtime

import (
	"context"
	"errors"
	"time"

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
// reverse unwind stays silent; anything the budget cut short names the exact
// plugins, because "the process exited" alone hides skipped cleanup.
func (a *App) reportShutdown(reason string, budget time.Duration) {
	abandoned := a.shutdownReport.Identities(assembly.StopAbandoned)
	notAttempted := a.shutdownReport.Identities(assembly.StopNotAttempted)
	if len(abandoned) == 0 && len(notAttempted) == 0 {
		return
	}
	a.log().Warn("xbc: shutdown budget expired before the reverse unwind finished",
		"reason", reason,
		"budget", budget.String(),
		"abandoned", identityLabels(abandoned),
		"not_attempted", identityLabels(notAttempted),
	)
}

func identityLabels(identities []plugin.Identity) []string {
	labels := make([]string, len(identities))
	for index, identity := range identities {
		labels[index] = identity.String()
	}
	return labels
}
