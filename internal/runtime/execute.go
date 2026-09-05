package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/cli"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

const (
	stopReasonSignal        = "signal"
	stopReasonContext       = "context"
	stopReasonCritical      = "critical"
	stopReasonCompleted     = "completed"
	stopReasonStartupFailed = "startup-failed"
)

// Execute drives this App once. Exit code 2 denotes command-line usage, 1 a
// planning/runtime failure, and 0 a complete doctor, migration, or clean run.
func (a *App) Execute(ctx context.Context, args []string) (int, error) {
	return a.execute(ctx, args, stopReasonContext)
}

func (a *App) execute(parent context.Context, args []string, cancelReason string) (int, error) {
	if parent == nil {
		return 1, fmt.Errorf("xbc: Execute context cannot be nil")
	}
	a.executeMu.Lock()
	if a.executed {
		a.executeMu.Unlock()
		return 1, fmt.Errorf("xbc: App.Execute can only be called once")
	}
	a.executed = true
	a.executeMu.Unlock()
	if err := parent.Err(); err != nil {
		return 1, fmt.Errorf("xbc: context was canceled before Execute: %w", err)
	}

	executionCtx, cancel := context.WithCancelCause(parent)
	a.stateMu.Lock()
	a.executionCtx = executionCtx
	a.cancelExec = cancel
	a.stateMu.Unlock()
	defer cancel(errors.New("xbc: execution finished"))
	stopWatching := context.AfterFunc(parent, func() { a.requestStop(cancelReason) })
	defer stopWatching()

	command, err := cli.ParseArgs(args)
	if err != nil {
		return 2, err
	}
	if err := a.ensureStarting("command parsing"); err != nil {
		return 1, err
	}
	if err := a.bootstrap(command); err != nil {
		return 1, err
	}
	if err := a.ensureStarting("bootstrapping"); err != nil {
		return 1, err
	}

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: a.bundles,
		Env:     a.env,
		Logger:  a.logger,
	})
	if err != nil {
		return 1, err
	}
	a.plan = plan
	migrate := command.WantsMigration(a.settings.AutoMigrate)
	if command.Subcommand == "doctor" {
		a.reportPlan(plan, migrate)
		return 0, nil
	}
	if len(plan.Order()) == 0 {
		return 1, a.errNothingEnabled(plan)
	}
	if err := a.ensureStarting("planning"); err != nil {
		return 1, err
	}

	owned, err := assembly.Construct(plan, assembly.ConstructOptions{
		ShutdownTimeout: a.settings.ShutdownTimeout,
		ContextFactory: func(identity plugin.Identity, logger log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(hostAdapter{app: a, logger: logger}, identity)
		},
	})
	if err != nil {
		return 1, err
	}
	a.owned = owned
	instances := owned.Instances()
	if err := a.ensureStarting("construction"); err != nil {
		return 1, a.abort(err)
	}

	if migrate {
		if err := a.migrateAll(instances); err != nil {
			return 1, a.abort(err)
		}
	}
	if command.Subcommand == "migrate" {
		if !a.requestStop(stopReasonCompleted) && a.currentStopReason() != stopReasonCompleted {
			return 1, a.abort(a.errStopDuringStartup("migration"))
		}
		if err := a.unwind(stopReasonCompleted); err != nil {
			return 1, err
		}
		return 0, nil
	}

	if err := a.startAll(instances); err != nil {
		return 1, a.abort(err)
	}
	if err := a.prepareTraffic(instances); err != nil {
		return 1, a.abort(err)
	}
	if !a.releaseTraffic() {
		return 1, a.abort(a.errStopDuringStartup("traffic gate release"))
	}
	if err := a.assertLiveness(instances); err != nil {
		return 1, a.abort(err)
	}
	a.reportStarted(instances, migrate)
	return a.wait()
}

func (a *App) ensureStarting(stage string) error {
	if a.stopRequested() {
		return a.errStopDuringStartup(stage)
	}
	return nil
}

// requestStop and releaseTraffic use the same mutex. Consequently a shutdown
// request cannot land between a successful check and an unconditional gate
// release: exactly one transition wins.
func (a *App) requestStop(reason string) bool {
	a.stateMu.Lock()
	if a.stopRequestedFlag {
		a.stateMu.Unlock()
		return false
	}
	a.stopRequestedFlag = true
	a.stopReason = reason
	close(a.stopCh)
	cancel := a.cancelExec
	tasks := a.tasks
	a.stateMu.Unlock()
	if tasks != nil {
		tasks.closeAdmission()
	}
	if cancel != nil {
		cancel(fmt.Errorf("xbc: stop requested: %s", reason))
	}
	return true
}

func (a *App) releaseTraffic() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.stopRequestedFlag {
		return false
	}
	if !a.trafficOpen {
		a.trafficOpen = true
		close(a.trafficGate)
	}
	return true
}

func (a *App) stopRequested() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.stopRequestedFlag
}

func (a *App) currentStopReason() string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.stopReason
}

func (a *App) onCritical(reason string) {
	a.log().Error("xbc: critical managed task requested shutdown", "reason", reason)
	a.requestStop(stopReasonCritical)
}

func (a *App) wait() (int, error) {
	if a.ready != nil {
		close(a.ready)
	}
	<-a.stopCh
	reason := a.currentStopReason()
	if err := a.unwind(reason); err != nil {
		return 1, err
	}
	if reason == stopReasonCritical || reason == stopReasonStartupFailed {
		return 1, fmt.Errorf("xbc: application stopped after %s failure", reason)
	}
	return 0, nil
}

func (a *App) assertLiveness(instances []*assembly.Instance) error {
	if a.tasks != nil && a.tasks.spawnedCount() > 0 {
		return nil
	}
	for _, instance := range instances {
		if instance.HasTrafficPreparation() {
			return nil
		}
	}
	labels := make([]string, len(instances))
	for index, instance := range instances {
		labels[index] = instance.Identity().String()
	}
	return fmt.Errorf("xbc: no plugin provides a long-lived capability; enabled plugins: %s", strings.Join(labels, ", "))
}

func (a *App) errNothingEnabled(plan *assembly.Plan) error {
	if plan.DefinitionCount() == 0 {
		return fmt.Errorf("xbc: no plugin was declared, nothing to do; compose Bundles explicitly or import an autoload leaf")
	}
	disabled := plan.Disabled()
	labels := make([]string, len(disabled))
	for index, key := range disabled {
		labels[index] = key.String()
	}
	return fmt.Errorf("xbc: declared %d plugins, but none were enabled; disabled: %s", plan.DefinitionCount(), strings.Join(labels, ", "))
}

func (a *App) log() log.Logger {
	if a.logger == nil {
		return log.L()
	}
	return a.logger
}
