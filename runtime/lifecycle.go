package runtime

import (
	"fmt"

	"github.com/xbcio/xbc/plugin/assembly"
)

func (a *App) migrateAll(instances []*assembly.Instance) error {
	for _, instance := range instances {
		if a.stopRequested() {
			return a.errStopDuringStartup("migration")
		}
		if !instance.HasMigration() {
			continue
		}
		if err := instance.InvokeMigration(); err != nil {
			return err
		}
	}
	return nil
}

// startAll opens task admission for exactly one Plugin while its synchronous
// Start hook executes, and closes it before observing the hook result.
func (a *App) startAll(instances []*assembly.Instance) error {
	for _, instance := range instances {
		if a.stopRequested() {
			return a.errStopDuringStartup("startup")
		}
		if !instance.HasStart() {
			continue
		}
		identity := instance.Identity()
		if !a.tasks.openStart(identity) {
			return a.errStopDuringStartup("startup")
		}
		err := instance.InvokeStart()
		a.tasks.closeStart(identity)
		if err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("startup")
		}
	}
	// The last Start hook has returned, which is the one moment a workload's
	// budget reading is at its most telling: submission of a managed task is
	// admitted only inside a Start hook, so every unit a workload's own plugins
	// will ever submit here has been submitted, while the queue workers and
	// cron runners that charge the admission half all wait on a traffic gate
	// that is still shut. A unit held now is start-up work's -- a resident loop,
	// or a task a Start hook spawned into a workload-scoped pool, which needs no
	// gate -- and the counter cannot tell those apart, which is why the report
	// warns rather than judges.
	a.reportSaturatedWorkloads()
	return nil
}

func (a *App) prepareTraffic(instances []*assembly.Instance) error {
	for _, instance := range instances {
		if a.stopRequested() {
			return a.errStopDuringStartup("traffic preparation")
		}
		if !instance.HasTrafficPreparation() {
			continue
		}
		if err := instance.InvokeTrafficPreparation(); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) errStopDuringStartup(stage string) error {
	return fmt.Errorf("xbc: received stop request during startup (%s), aborting %s phase and starting reverse cleanup", a.currentStopReason(), stage)
}
