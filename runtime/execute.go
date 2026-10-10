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
// planning/runtime failure, and 0 a complete doctor, validate, migration,
// consumer-defined command, or clean run.
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

	command, err := parseArgs(args, config.DefaultEnvPrefix, a.commands)
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

	// A consumer-defined subcommand runs here, and only here: after bootstrap,
	// so it reads the merged configuration through the same strict decoding a
	// plugin's input gets and logs through the installed logger, and before
	// everything else a boot does. Nothing below this branch has happened for
	// it -- placement was not resolved, no factory ran, no migration ran, no
	// listener was opened, no task was admitted -- which is what lets a
	// management command run against a process that must not consume cluster
	// capacity or serve traffic. A command that needs a dependency opens and
	// closes its own connection; the failure it returns is reported as its own,
	// with the command named, and exits 1.
	if subcommand, ok := a.customCommand(command.subcommand); ok {
		a.progress.enterPhase(phaseCommand)
		if err := subcommand.run(executionCtx, a.env, command.args); err != nil {
			return 1, fmt.Errorf("xbc: command %q failed: %w", subcommand.name, err)
		}
		return 0, nil
	}

	a.progress.enterPhase(phasePlanning)
	planningStarted := time.Now()
	// A placement source may acquire something to answer -- the lease source
	// wins its slots inside Resolve -- and it is given back through
	// plugin.PlacementReleaser. This deferred call is the run's backstop for
	// that. On a running application it fires after wait and unwind, so the
	// placement plugin's own PreStop and Stop hooks have already had their
	// chance and this release is the idempotent no-op the contract requires it
	// to be; on every other path out of here it is the only thing that gives the
	// claim back at all, because nothing else runs: doctor and a plan failure
	// construct nothing, a plan that enables nothing never reaches Construct, a
	// stop during planning returns the same way, a composition that never
	// selected the placement Bundle has no hook to run, and a shutdown the
	// budget abandoned never ran Stop. Without it each of those would hold the
	// claim until its lease expired, so a read-only diagnostic would consume
	// cluster capacity and a stopped process would delay the next takeover by a
	// full TTL. Disarming it after Construct was tried and is wrong: a graph
	// without the placement plugin in it looks exactly like one that releases,
	// from here. A failed release is a warning for every path that has a logger;
	// doctor runs against a no-op one, so its report is where the failure has to
	// land instead.
	defer func() {
		if releaseErr := a.releasePlacement(executionCtx); releaseErr != nil && command.subcommand == doctorSubcommand {
			a.reportDoctorPlacementRelease(releaseErr)
		}
	}()
	// The hosted set is settled before the plan is built, not while it is
	// being built: a workload this process does not carry must contribute no
	// Definition at all, so the decision has to exist before planning starts.
	// A source that cannot answer fails the run rather than defaulting, because
	// every downstream decision -- what is constructed, what doctor reports,
	// which exclusive constraints apply -- is derived from this one.
	placement, err := a.resolvePlacement(executionCtx)
	if err != nil {
		return 1, err
	}
	a.logger.Info("xbc: placement resolved by "+placement.Source,
		"instance", a.settings.Instance(),
		"hosted", workloadLabels(placement.Hosted))
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles:   a.bundles,
		Env:       a.env,
		Logger:    a.logger,
		Placement: placement,
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
	// Ownership of whatever the placement source acquired moves to the plugin
	// graph here: the placement plugin's PreStop and Stop hooks are what give it
	// back on a well-formed stop, and the deferred release at the top of execute
	// is the backstop for the paths they cannot cover.
	phases.construct = time.Since(constructStarted)
	a.owned = owned
	instances := owned.Instances()
	if err := a.ensureStarting("construction"); err != nil {
		return 1, a.abort(err)
	}

	// validate is a constructing command that serves nothing: it runs every
	// Preflight hook -- the assembly a Start would perform, without activating
	// anything -- and then unwinds. It sits above the migration block on
	// purpose: validate never migrates, so neither --migrate nor
	// xbc.auto_migrate is consulted on this path, and a startup mistake fails
	// the command before any schema change could run.
	if command.subcommand == validateSubcommand {
		return a.validate(instances, startedAt)
	}

	if migrate {
		a.progress.enterPhase(phaseMigrate)
		migrateStarted := time.Now()
		if err := a.migrateAll(instances); err != nil {
			return 1, a.abort(err)
		}
		phases.migrate = time.Since(migrateStarted)
	}
	if command.subcommand == migrateSubcommand {
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
	// The process is servable: every Start hook has returned, the traffic
	// gate is open, and something will keep the process busy. The
	// configuration watch starts here and nowhere earlier -- a reload must
	// never race plugin startup or interleave with the startup report -- and
	// before wait, so wait and every path that reaches unwind hand it the
	// same watch state, and a run that observed its running state is already
	// watching.
	a.startConfigWatch()
	return a.wait()
}

// validate is the validate command: every Preflight hook in graph order, then
// the command's own report, then the ordinary reverse stop walk. It serves
// nothing -- no Start hook runs, no traffic gate is released -- and it is the
// only caller of preflightAll.
//
// The report is emitted before the unwind, exactly as the released-gate line
// is: the number it states is how long validation took, not how long the
// process lived, and a stop that fails afterwards is reported as its own error.
func (a *App) validate(instances []*assembly.Instance, startedAt time.Time) (int, error) {
	a.progress.enterPhase(phaseValidate)
	if err := a.preflightAll(instances); err != nil {
		return 1, a.abort(err)
	}
	// preflightAll checks for a stop request before every hook, which leaves
	// the tail uncovered: a request landing after the last hook returned -- or
	// a composition that declares no Preflight at all -- would otherwise be
	// recorded as a completed validation. Claiming the completion stop is what
	// tells the two apart, exactly as the migrate subcommand does, and an
	// external request keeps its own reason, so the report names what actually
	// stopped the run.
	if !a.requestStop(stopReasonCompleted) && a.currentStopReason() != stopReasonCompleted {
		return 1, a.abort(a.errStopDuringStartup("validation"))
	}
	a.validation = time.Since(startedAt)
	a.reportValidated(instances)
	a.reportValidationTimings(instances)
	if err := a.unwind(stopReasonCompleted); err != nil {
		return 1, err
	}
	return 0, nil
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
	// This is where admission usually closes, under the same lock that
	// records the stop, so a task can never be admitted after the stop is
	// observable. unwind (shutdown.go) also closes admission, because a stop
	// arriving before the task runtime is published finds no runtime here and
	// this call is then a no-op; see its comment. Do not move it below the
	// unlock without updating that.
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
	// DefinitionCount counts the graph that was built, and placement filters the
	// graph before it is built. So a zero here has two causes that call for
	// opposite fixes: nothing was declared at all, or nothing remained after
	// placement removed every workload-scoped Definition. Telling the second as
	// "nothing to do" sends the operator to edit Bundles when the thing to
	// change is placement. The presence of an unhosted workload is what
	// distinguishes the two -- it does not by itself prove placement removed
	// anything, but it is the only sign that the composition declared more than
	// the empty graph shows, and it is what an operator can act on.
	if plan.DefinitionCount() == 0 {
		if unhosted := unhostedWorkloads(plan.Workloads()); len(unhosted) > 0 {
			return fmt.Errorf(
				"xbc: no plugin entered the graph; workload(s) %s are not carried by this process, and no other plugin was declared here. Placement decides which workloads a process hosts -- set workloads.<key>.enabled: true, or check what the placement source decided",
				strings.Join(unhosted, ", "))
		}
		return fmt.Errorf("xbc: no plugin was declared, nothing to do; compose Bundles explicitly or import an autoload leaf")
	}
	disabled := plan.Disabled()
	labels := make([]string, len(disabled))
	for index, key := range disabled {
		labels[index] = key.String()
	}
	return fmt.Errorf("xbc: this process carries %d plugin(s), but none were enabled; disabled: %s", plan.DefinitionCount(), strings.Join(labels, ", "))
}

// unhostedWorkloads names the declared workloads this process does not carry,
// sorted by key as the plan reports them. It is what tells a placement-shaped
// empty graph apart from a composition that declared nothing.
func unhostedWorkloads(workloads []assembly.PlanWorkload) []string {
	keys := make([]string, 0, len(workloads))
	for _, workload := range workloads {
		if !workload.Hosted {
			keys = append(keys, workload.Workload.Key.String())
		}
	}
	return keys
}

func (a *App) log() log.Logger {
	if a.logger == nil {
		return log.L()
	}
	return a.logger
}
