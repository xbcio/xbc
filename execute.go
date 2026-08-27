package xbc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/internal/cli"
	"github.com/xbcio/xbc/internal/container"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Why a run stops. The reason is recorded once, by whoever gets there first,
// and read afterwards to decide both what to log and what exit code the
// process leaves behind.
const (
	// stopReasonSignal is an operator- or orchestrator-requested stop
	// (SIGINT/SIGTERM). An intentional stop: exit code 0.
	stopReasonSignal = "signal"

	// stopReasonContext is a caller-requested stop delivered through the
	// context passed to Execute. Like an OS signal after startup, this is an
	// intentional stop and therefore exits cleanly when unwind succeeds.
	stopReasonContext = "context"

	// stopReasonCritical is a GoCritical task that panicked or returned
	// without being asked to. Nobody requested this stop: exit code 1.
	stopReasonCritical = "critical"

	// stopReasonCompleted is a one-shot run (the migrate subcommand) that
	// finished its work. It is not an external stop request, but ending the
	// execution still cancels its plugin Contexts before unwind begins.
	stopReasonCompleted = "completed"
)

// Execute drives the application using args until it completes or ctx is
// cancelled. It never reads os.Args, registers process signals, calls
// log.Sync, or terminates the process; callers embedding xbc retain ownership
// of those process-wide concerns. An App is single-use and Execute must be
// called at most once.
//
// Exit codes: 2 is a command-line usage error (the flag package's own
// convention), 1 is any runtime or assembly failure, 0 is a clean run.
func (a *App) Execute(ctx context.Context, args []string) (int, error) {
	return a.execute(ctx, args, stopReasonContext)
}

// execute is shared by Execute and the process-level Run adapter. The latter
// supplies stopReasonSignal so diagnostics retain the actual process trigger.
func (a *App) execute(ctx context.Context, args []string, cancelReason string) (int, error) {
	if ctx == nil {
		return 1, fmt.Errorf("xbc: Execute 的 context 不能为空")
	}
	a.executeMu.Lock()
	if a.executed {
		a.executeMu.Unlock()
		return 1, fmt.Errorf("xbc: App.Execute 只能调用一次")
	}
	a.executed = true
	a.executeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 1, fmt.Errorf("xbc: Execute 开始前 context 已取消：%w", err)
	}
	stopContext := a.watchContext(ctx, cancelReason)
	defer stopContext()

	cmd, err := cli.ParseArgs(args)
	if err != nil {
		return 2, err
	}
	if a.stopRequested() {
		return 1, a.errStopDuringStartup("命令解析")
	}

	if err := a.bootstrap(cmd); err != nil {
		return 1, err
	}
	if a.stopRequested() {
		return 1, a.errStopDuringStartup("引导")
	}

	if err := a.container.Assemble(); err != nil {
		return 1, err
	}
	order := a.container.Order()
	a.bindLifecycleContexts(ctx, order)
	migrate := cmd.WantsMigration(a.settings.AutoMigrate)
	if a.stopRequested() {
		return 1, a.errStopDuringStartup("装配")
	}

	// doctor reports the assembly result and stops there: it must not
	// construct a single connection, so it returns before Init or any other
	// resource-creating lifecycle hook. Execute itself never owns a signal
	// handler; the process adapter installs one before entering this execution
	// flow.
	if cmd.Subcommand == "doctor" {
		a.report(order, migrate)
		return 0, nil
	}

	if len(order) == 0 {
		return 1, a.errNothingEnabled()
	}

	if err := a.initAll(order); err != nil {
		return 1, a.abort(err)
	}
	if migrate {
		if err := a.migrateAll(order); err != nil {
			return 1, a.abort(err)
		}
		// Close the last-in-the-stage race for a one-shot migrate run: there
		// may be no next lifecycle iteration to observe a cancellation that
		// arrived just after migrateAll's final per-instance check.
		if a.stopRequested() {
			return 1, a.abort(a.errStopDuringStartup("迁移"))
		}
	}

	// The migrate subcommand is a one-shot run. It still unwinds in full:
	// a Migrator's Init may have opened a pool or started a managed task,
	// and "the process is about to exit anyway" is not a reason to leave a
	// half-written transaction or an unflushed buffer behind.
	if cmd.Subcommand == "migrate" {
		// Completion participates in the same first-reason-wins transition as
		// every external stop. This closes the gap between migrateAll's final
		// cancellation check and unwind: if context/signal/critical wins that
		// race, startup was interrupted and must still exit 1; if completed wins,
		// a later stop cannot relabel the successful one-shot run.
		a.requestStop(stopReasonCompleted)
		if a.stopReason != stopReasonCompleted {
			return 1, a.abort(a.errStopDuringStartup("迁移"))
		}
		if err := a.unwind(stopReasonCompleted); err != nil {
			return 1, err
		}
		return 0, nil
	}

	a.report(order, migrate)

	if err := a.startRunners(order); err != nil {
		return 1, a.abort(err)
	}
	if err := a.openTraffic(order); err != nil {
		return 1, a.abort(err)
	}
	if err := a.assertLiveness(order); err != nil {
		return 1, a.abort(err)
	}
	if a.stopRequested() {
		return 1, a.abort(a.errStopDuringStartup("完成启动"))
	}

	return a.wait()
}

// executionLifecycleContext preserves caller values/deadlines while making
// the App's stop channel the single cancellation signal observed by every
// plugin Context. Caller cancellation reaches it through watchContext;
// process signals reach it through executeWithSignals -> requestStop.
type executionLifecycleContext struct {
	parent context.Context
	done   <-chan struct{}
	reason *string
}

func (c *executionLifecycleContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c *executionLifecycleContext) Done() <-chan struct{}       { return c.done }
func (c *executionLifecycleContext) Value(key any) any           { return c.parent.Value(key) }

func (c *executionLifecycleContext) Err() error {
	select {
	case <-c.done:
		// Only caller-driven cancellation inherits the parent's exact cause.
		// A signal/critical stop remains context.Canceled even if the caller's
		// deadline expires later, preserving context.Context's guarantee that
		// Err is stable after Done closes.
		if c.reason != nil && *c.reason == stopReasonContext {
			if err := c.parent.Err(); err != nil {
				return err
			}
		}
		return context.Canceled
	default:
		return nil
	}
}

// watchContext links caller cancellation to the App's single stop path. It
// uses context.AfterFunc so Execute owns no permanently blocked watcher
// goroutine, and the returned stop function detaches the callback on every
// early-return path (including doctor and argument errors).
func (a *App) watchContext(ctx context.Context, reason string) func() {
	stop := context.AfterFunc(ctx, func() { a.requestStop(reason) })
	return func() { stop() }
}

// wait blocks until something asks the application to stop, then unwinds.
//
// A shutdown that could not finish within the budget exits non-zero even
// when the stop itself was requested. "SIGTERM received, some plugins never
// released their resources" is not a clean stop, and reporting it as one
// would hide from the orchestrator the single thing it can act on: that this
// container needed to be killed rather than asked.
func (a *App) wait() (int, error) {
	if a.ready != nil {
		close(a.ready)
	}
	<-a.stopCh

	reason := a.stopReason
	if err := a.unwind(reason); err != nil {
		return 1, err
	}
	if reason == stopReasonCritical {
		return 1, nil
	}
	return 0, nil
}

// requestStop records why the execution is ending, cancels every bound plugin
// lifecycle Context by closing stopCh, and only then allows Execute to advance
// into unwind. It covers external requests (signal/context), critical failure,
// startup failure, and orderly one-shot completion alike. Safe to call from any
// goroutine, any number of times: the first caller wins and every later one is
// a no-op, so a signal arriving in the middle of a critical-triggered shutdown
// cannot close stopCh twice or replace the original reason.
//
// Startup hooks run synchronously on Execute's goroutine. Consequently, when
// requestStop closes stopCh during a hook, unwind cannot begin until that hook
// observes ctx.Done() and returns; Stop is never raced against an in-flight
// Init/Migrate/Start/OpenTraffic call.
//
// stopReason is written inside the Once, before the close; every reader
// reads it only after observing the close, so the channel close provides the
// happens-before edge and no additional lock is needed.
func (a *App) requestStop(reason string) {
	a.stopOnce.Do(func() {
		a.stopReason = reason
		close(a.stopCh)
	})
}

// stopRequested reports whether a stop has been requested. Startup operations
// poll it between steps -- that is what makes cancellation during Init or
// Runner.Start stop the remaining operations instead of being
// noticed only once everything has finished starting.
func (a *App) stopRequested() bool {
	select {
	case <-a.stopCh:
		return true
	default:
		return false
	}
}

// onCritical is the escalation path handed to the task group: a managed
// critical goroutine panicked or returned unprompted.
//
// It records the reason and cancels the execution-scoped lifecycle Context
// immediately, so a synchronous startup hook can cooperate and return. It
// does not cancel managed-task contexts or unwind by itself: both remain the
// responsibility of the single unwind path, after any in-flight hook has
// returned, so Stop cannot race plugin startup code.
func (a *App) onCritical(reason string) {
	a.log().Error("xbc: 收到 critical 信号，即将关闭应用", "reason", reason)
	a.requestStop(stopReasonCritical)
}

// assertLiveness refuses to hand control to the run loop when nothing in the
// application would keep it busy.
//
// Without this, an application whose plugins all finished their work during
// Init blocks on stopCh forever: nothing is serving, no task is running,
// nothing will ever request a stop. The process looks healthy to every
// external probe -- it is up, it is not consuming CPU -- while doing
// precisely nothing. A container orchestrator has no way to tell that apart
// from a working service. Failing loudly at startup does.
//
// Both halves of the readiness protocol count. A plugin that implements only
// TrafficOpener -- because it has nothing to prepare and simply starts
// accepting -- is as much a live service as one that implements Runner, and
// requiring Runner specifically would reject it for a reason that is about
// interface shape rather than about whether the application does anything.
//
// The test is "is there something that serves, or has any managed task ever
// been admitted", not "is a managed task running right now". The second
// question cannot be asked without a race: a legitimate long-lived task may
// not have been scheduled yet at this instant. The first is answerable and is
// the question that actually distinguishes an application built to stay up
// from one that was not.
func (a *App) assertLiveness(order []*container.Instance) error {
	for _, inst := range order {
		switch inst.Plugin().(type) {
		case plugin.Runner, plugin.TrafficOpener:
			return nil
		}
	}
	if a.tasks.spawnedCount() > 0 {
		return nil
	}

	names := make([]string, 0, len(order))
	for _, inst := range order {
		names = append(names, inst.Label())
	}
	return fmt.Errorf(
		"xbc: 没有任何插件提供长期存活能力，应用启动后会永远空等\n"+
			"  已启用的插件：%s\n"+
			"  → 需要一个实现 Start(ctx) 的 Runner（例如 blank import 一个服务端模块），"+
			"或在 Init 中通过 ctx.Go/ctx.GoCritical 启动长期任务",
		strings.Join(names, ", "))
}

// errNothingEnabled explains an assembly that produced zero instances. The
// two causes read identically from the outside -- the process starts and
// immediately fails -- but need opposite fixes, so the message distinguishes
// them by what the catalog actually held.
func (a *App) errNothingEnabled() error {
	if a.snapshot.Len() == 0 {
		return fmt.Errorf(
			"xbc: 没有任何插件被声明，无事可做\n" +
				"  → 检查是否忘了 blank import 插件的 autoload 包（例如 _ \"github.com/xbcio/xbc/web/autoload\"）")
	}
	disabled := a.container.Disabled()
	return fmt.Errorf(
		"xbc: 声明了 %d 个插件，但没有一个被启用\n"+
			"  未启用：%s\n"+
			"  → 这些插件按 Activation 需要对应的配置节；检查配置文件路径（--config）和 plugins.* 小节是否写对",
		a.snapshot.Len(), strings.Join(disabled, ", "))
}

// log returns the App's logger, falling back to the global one so a failure
// raised before bootstrap finished still has somewhere to go.
func (a *App) log() log.Logger {
	if a.logger == nil {
		return log.L()
	}
	return a.logger
}
