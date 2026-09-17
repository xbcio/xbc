package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
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

	command, err := parseArgs(args, config.DefaultEnvPrefix)
	if err != nil {
		return 2, err
	}
	if err := a.ensureStarting("command parsing"); err != nil {
		return 1, err
	}
	// startedAt anchors the one number an operator always wants: how long the
	// process took to reach servable. It is taken after argument parsing has
	// succeeded, because a usage error never boots anything.
	startedAt := time.Now()
	var phases startupPhases
	// Recorded before the watchdog exists, so that the sliver between starting
	// it and entering planning reports the phase that has genuinely just been
	// running rather than no phase at all.
	a.progress.enterPhase(phaseBootstrap)
	bootstrapStarted := time.Now()
	if err := a.bootstrap(command); err != nil {
		return 1, err
	}
	phases.bootstrap = time.Since(bootstrapStarted)
	// The watchdog cannot start any earlier than this: its interval is a
	// configured setting, so bootstrap has to produce it first, and there is
	// no logger to report through until bootstrap has installed one. A boot
	// that hangs inside configuration loading is therefore out of its reach;
	// every phase after it is covered. The deferred stop is the backstop for
	// every failure return below; the successful path stops it explicitly
	// before announcing the released gate.
	stopWatch := a.watchSlowStartup(a.log(), startedAt, a.settings.SlowStartupAfter)
	defer stopWatch()
	if err := a.ensureStarting("bootstrapping"); err != nil {
		return 1, err
	}

	a.progress.enterPhase(phasePlanning)
	planningStarted := time.Now()
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: a.bundles,
		Env:     a.env,
		Logger:  a.logger,
	})
	if err != nil {
		return 1, err
	}
	phases.planning = time.Since(planningStarted)
	a.plan = plan
	migrate := command.wantsMigration(a.settings.AutoMigrate)
	if command.subcommand == doctorSubcommand {
		a.reportDoctor(plan, migrate)
		return 0, nil
	}
	a.reportDisabled(plan)
	if len(plan.Order()) == 0 {
		return 1, a.errNothingEnabled(plan)
	}
	if err := a.ensureStarting("planning"); err != nil {
		return 1, err
	}

	a.progress.enterPhase(phaseConstruct)
	constructStarted := time.Now()
	owned, err := assembly.Construct(plan, assembly.ConstructOptions{
		ShutdownTimeout: a.settings.ShutdownTimeout,
		ContextFactory: func(identity plugin.Identity, logger log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(hostAdapter{app: a, logger: logger}, identity)
		},
		// One record per stage, written before the hook runs. Every phase from
		// here on hands control to plugin code that may not come back, and
		// this is what names the plugin holding it.
		OnStageBegin: a.progress.enterStage,
	})
	if err != nil {
		return 1, err
	}
	phases.construct = time.Since(constructStarted)
	a.owned = owned
	instances := owned.Instances()
	if err := a.ensureStarting("construction"); err != nil {
		return 1, a.abort(err)
	}

	if migrate {
		a.progress.enterPhase(phaseMigrate)
		migrateStarted := time.Now()
		if err := a.migrateAll(instances); err != nil {
			return 1, a.abort(err)
		}
		phases.migrate = time.Since(migrateStarted)
	}
	if command.subcommand == "migrate" {
		if !a.requestStop(stopReasonCompleted) && a.currentStopReason() != stopReasonCompleted {
			return 1, a.abort(a.errStopDuringStartup("migration"))
		}
		if err := a.unwind(stopReasonCompleted); err != nil {
			return 1, err
		}
		return 0, nil
	}

	a.progress.enterPhase(phaseStart)
	startStarted := time.Now()
	if err := a.startAll(instances); err != nil {
		return 1, a.abort(err)
	}
	phases.start = time.Since(startStarted)
	a.progress.enterPhase(phaseTraffic)
	trafficStarted := time.Now()
	if err := a.prepareTraffic(instances); err != nil {
		return 1, a.abort(err)
	}
	phases.openTraffic = time.Since(trafficStarted)
	if !a.releaseTraffic() {
		return 1, a.abort(a.errStopDuringStartup("traffic gate release"))
	}
	if err := a.assertLiveness(instances); err != nil {
		return 1, a.abort(err)
	}
	total := time.Since(startedAt)
	a.startup = startupTiming{total: total, phases: phases}
	// Startup is over, so the watchdog is joined before anything announces
	// that: a report claiming startup has not finished must not be able to
	// land after the line saying it has.
	stopWatch()
	a.reportStarted(instances, migrate)
	a.reportStartupTimings(instances)
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
// release: exactly one transition wins. It also cancels the execution context
// before publishing stopCh, so readiness consumers observe shutdown before
// reverse cleanup can begin draining a transport.
func (a *App) requestStop(reason string) bool {
	a.stateMu.Lock()
	if a.stopRequestedFlag {
		a.stateMu.Unlock()
		return false
	}
	a.stopRequestedFlag = true
	a.stopReason = reason
	cancel := a.cancelExec
	tasks := a.tasks
	if tasks != nil {
		tasks.closeAdmission()
	}
	if cancel != nil {
		cancel(fmt.Errorf("xbc: stop requested: %s", reason))
	}
	close(a.stopCh)
	a.stateMu.Unlock()
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

// assertLiveness refuses to hand a process to wait() when nothing will keep it
// busy. Only two contributions qualify: opening traffic, or a critical managed
// task. A non-critical task is contractually allowed to return, so it cannot
// justify a process that would then block until a signal with no work left.
func (a *App) assertLiveness(instances []*assembly.Instance) error {
	if a.tasks != nil && a.tasks.criticalTaskCount() > 0 {
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
	return fmt.Errorf("xbc: no plugin provides a long-lived capability; open traffic or submit a critical managed task from Start; enabled plugins: %s", strings.Join(labels, ", "))
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
