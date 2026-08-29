package runtime

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/xbcio/xbc/assembly"
	"github.com/xbcio/xbc/plugin"
)

// bindLifecycleContexts gives every instance a fresh execution-scoped
// context before any startup hook can run. The App is single-use, and these
// Context values are newly constructed during this Execute's assembly, so a
// cancelled context can never leak into another App or a later execution.
func (a *App) bindLifecycleContexts(parent context.Context, order []*assembly.Instance) {
	executionCtx := &executionLifecycleContext{parent: parent, done: a.stopCh, reason: &a.stopReason}
	for _, inst := range order {
		plugin.BindLifecycleContext(inst.Context(), executionCtx)
	}
}

// callStartupHook is the single boundary around plugin-owned startup code.
// Hooks remain synchronous: recover converts a panic into an ordinary startup
// error, after which Execute uses the same abort/unwind path as a returned
// error. That ordering is what guarantees initialized plugins are stopped
// without ever racing Stop against the panicking hook.
func callStartupHook(
	inst *assembly.Instance,
	stage string,
	hook string,
	call func() error,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			id := inst.Identity()
			err = fmt.Errorf(
				"xbc: plugin %s %s failed: %s hook panic (key=%q, instance=%q): %v\n%s",
				inst.Label(), stage, hook, id.Plugin, id.Instance, recovered, debug.Stack(),
			)
		}
	}()

	if err := call(); err != nil {
		return fmt.Errorf("xbc: plugin %s %s failed: %w", inst.Label(), stage, err)
	}
	return nil
}

// initAll runs the initialization stage: for every instance, in dependency
// order, inject its dependencies, call Init if it has one, then harvest the
// values it promised to produce.
//
// The instance is marked initialized between Init and Harvest, not after
// both. Once Init has returned successfully the plugin may already hold a
// live resource, so from that moment on it must be in the set that unwind
// stops -- a harvest failure immediately afterwards has to release it, not
// leak it.
//
// The Context handed to Init is the one the assembly container already built
// and bound
// to the plugin's embedded Base during expansion. This stage never mints a
// second one; that is what keeps Base.Ctx() and the ctx parameter from ever
// drifting apart.
func (a *App) initAll(order []*assembly.Instance) error {
	for _, inst := range order {
		if a.stopRequested() {
			return a.errStopDuringStartup("initialization")
		}

		if err := a.container.Inject(inst); err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("initialization")
		}

		if initer, ok := inst.Plugin().(plugin.Initializer); ok {
			if err := callStartupHook(inst, "initialization", "Init", func() error {
				return initer.Init(inst.Context())
			}); err != nil {
				return err
			}
		}
		// A successful Init transfers resource ownership to the framework.
		// Mark before checking cancellation or harvesting so every later failure
		// is guaranteed to call this instance's Stop.
		a.container.MarkInitialized(inst)
		if a.stopRequested() {
			return a.errStopDuringStartup("initialization")
		}

		if err := a.container.Harvest(inst); err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("initialization")
		}
	}

	// Sealing is what makes plugin.Extensions[T] start answering. Until this
	// line, a call to it returns an error rather than a partial set -- see
	// Container.InitializedPlugins.
	a.container.SealInitialization()
	return nil
}

// migrateAll runs the migration stage over every Migrator, in dependency
// order. It runs only when the run was told to migrate (--migrate, the
// migrate subcommand, or xbc.auto_migrate): migration is a side-effecting
// write, and binding it to every boot would mean every rolling restart
// silently touches the schema.
func (a *App) migrateAll(order []*assembly.Instance) error {
	for _, inst := range order {
		if a.stopRequested() {
			return a.errStopDuringStartup("migration")
		}
		migrator, ok := inst.Plugin().(plugin.Migrator)
		if !ok {
			continue
		}
		if err := callStartupHook(inst, "migration", "Migrate", func() error {
			return migrator.Migrate(inst.Context())
		}); err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("migration")
		}
	}
	return nil
}

// startRunners runs the first half of the two-phase readiness protocol:
// every Runner prepares and binds, in dependency order, and this returns only
// once all of them have.
//
// The split between Start and OpenTraffic is what makes readiness meaningful
// without the core knowing anything about protocols. Start does everything
// that can fail -- claim the port, dial the broker, build the route table --
// and returns without serving a single request. Only after every Runner has
// cleared that bar does openTraffic let any of them accept traffic. So the
// failure mode where an HTTP server is already answering health checks while
// the gRPC server next to it is still failing to bind, and the orchestrator
// routes production traffic into a half-built application, cannot happen: the
// bind failure is discovered while nothing is serving yet, and the whole app
// unwinds instead.
//
// This operation is sequential, in the same dependency order as every other
// lifecycle operation.
// Running Starts concurrently would buy nothing -- Start is required to return
// promptly, so there is no latency to hide -- and would cost two real things:
// a Runner could no longer assume the plugins it depends on have finished
// binding, and a bind failure would arrive interleaved with other plugins'
// output instead of naming one plugin unambiguously. Failing fast on the first
// error is safe for the same reason the whole startup path is: the caller
// unwinds, which stops everything that did start.
//
// The core's only knowledge of who participates is a type assertion. There is
// no list of protocol names and no hard-coded "web goes first" rule. The
// dependency topology supplies semantic order; Definition-key order only makes
// otherwise-unconstrained ties deterministic. A plugin that implements Runner
// joins the barrier, and one that does not is not waited for. That is the entire
// mechanism, and it is why adding a
// gRPC or a message-consumer module later requires no change here.
//
// A Runner's long-lived loop belongs in a managed task (ctx.Go /
// ctx.GoCritical), which is also what makes it participate in bounded
// shutdown.
func (a *App) startRunners(order []*assembly.Instance) error {
	for _, inst := range order {
		if a.stopRequested() {
			return a.errStopDuringStartup("startup")
		}
		runner, ok := inst.Plugin().(plugin.Runner)
		if !ok {
			continue
		}
		if err := callStartupHook(inst, "startup", "Start", func() error {
			return runner.Start(inst.Context())
		}); err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("startup")
		}
	}
	return nil
}

// openTraffic runs the second half of the readiness protocol: every
// TrafficOpener starts accepting, now that every Runner is known to have
// bound successfully.
func (a *App) openTraffic(order []*assembly.Instance) error {
	for _, inst := range order {
		if a.stopRequested() {
			return a.errStopDuringStartup("open traffic")
		}
		opener, ok := inst.Plugin().(plugin.TrafficOpener)
		if !ok {
			continue
		}
		if err := callStartupHook(inst, "open traffic", "OpenTraffic", func() error {
			return opener.OpenTraffic(inst.Context())
		}); err != nil {
			return err
		}
		if a.stopRequested() {
			return a.errStopDuringStartup("open traffic")
		}
	}
	return nil
}

// errStopDuringStartup describes startup cut short by caller cancellation, a
// process signal, or a critical failure. It is an error rather than a quiet
// early return because
// the run did not do what it was asked to do -- the process must exit
// non-zero, and the operator needs to see which stage was interrupted.
func (a *App) errStopDuringStartup(stage string) error {
	return fmt.Errorf("xbc: received stop request during startup (%s), aborting %s phase and starting reverse cleanup", a.stopReason, stage)
}
