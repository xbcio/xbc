package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/xbcio/xbc/plugin"
)

func (a *App) abort(cause error) error {
	if err := a.unwind(stopReasonStartupFailed); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// unwind is idempotent and applies one shared budget to the complete reverse
// walk. Constructed.Unwind invokes Stop before afterStop cancels and joins the
// same Plugin's task scope.
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
			err := a.owned.Unwind(deadline, budget, func(identity plugin.Identity) error {
				if a.tasks == nil {
					return nil
				}
				return a.tasks.stopPlugin(identity, deadline)
			})
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
	})
	return a.unwindErr
}
