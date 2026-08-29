package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// This file tests taskRuntime (task.go) against package-layout design §5.4
// (admission gate) and §5.5 (global shuttingDown flag), split between two
// levels on purpose:
//
//   - Direct taskRuntime tests, built with newTaskRuntime, for the exact
//     semantics that would be drowned out by App's own moving parts
//     (spawnedCount, per-plugin context isolation, the Add/Wait race, the
//     shuttingDown flag itself).
//   - Full App tests, run through runAsync/awaitResult, for the wiring that
//     only exists once a real hostAdapter and a real doUnwind are in play
//     (Context.TasksAccepted, exit codes, drainRemaining reached from a
//     failed Init).
//
// §5.5's own pinning test ("a plugin's Stop makes its critical goroutine
// return, and the whole run must still exit 0") is deliberately NOT
// duplicated here from the exit-code/log angle -- that is shutdown_test.go's
// job. This file covers the same rule from taskRuntime's internal state
// instead (TestTaskShuttingDownFlagChangesWhetherReturnIsJudgedUnexpected).

// taskCaptureLogger builds a real *log.Logger backed by a JSON file in the
// test's temp dir, and a logCapture handle to read it back.
//
// It exists because a bare taskRuntime is built directly by newTaskRuntime,
// never through App.bootstrap -- so harness_test.go's captureLogs (which
// only hands back a config fragment for config.Environment to bind) is not
// reachable here. This calls log.Init itself, the same assembly step
// bootstrap would otherwise perform.
func taskCaptureLogger(t *testing.T) (log.Logger, *logCapture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.jsonl")

	cfg := log.DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = path
	require.NoError(t, log.Init(cfg), "初始化任务运行时测试日志失败")

	return log.L(), &logCapture{path: path}
}

// --- §5.4: admission gate -------------------------------------------------

// TestTaskSubmitRejectedAfterAdmissionClosedLogsIdentifiedWarn pins the
// contract in §5.4: a submission after closeAdmission must be refused, not
// silently dropped, and the refusal must be observable both by the caller
// (submit's own bool return) and by an operator reading logs (a WARN naming
// which plugin instance was refused).
//
// A weaker implementation that just returns false without logging, or logs
// without the plugin identity, would pass a "submit returns false" check
// but leave an operator with no way to tell which of many plugins tried to
// spawn work during shutdown -- exactly the silent-drop failure mode §5.4's
// design doc calls out.
func TestTaskSubmitRejectedAfterAdmissionClosedLogsIdentifiedWarn(t *testing.T) {
	logger, cap := taskCaptureLogger(t)
	rt := newTaskRuntime(logger, func(reason string) {
		t.Fatalf("被拒绝的普通提交不应触发 critical，reason=%q", reason)
	})
	id := plugin.Identity{Plugin: "billing", Instance: "eu"}

	rt.closeAdmission()

	var ran atomic.Bool
	ok := rt.submit(id, func(context.Context) { ran.Store(true) }, false)

	require.False(t, ok, "准入关闭后 submit 必须返回 false，调用方才有办法知道任务没有启动")
	assert.False(t, ran.Load(), "被拒绝的任务不能仍然启动 goroutine——那才是真正的静默丢弃")

	var found bool
	for _, e := range cap.entries(t) {
		if e.Level == "warn" && strings.Contains(e.Message, "任务组已关闭") && e.Plugin == id.String() {
			found = true
		}
	}
	assert.True(t, found, "拒绝提交必须产生一条写明插件身份（%s）的 WARN 日志，不能静默丢弃", id.String())
}

// TestTaskConcurrentSubmitDuringCloseAdmissionAndStopDoesNotRace is the
// structural pinning test for the Add/Wait race §5.4 exists to eliminate.
//
// 50 goroutines hammer submit for one plugin identity while the main
// goroutine runs exactly doUnwind's own two-step sequence for that plugin:
// closeAdmission then stopPlugin (cancel + wg.Wait). An implementation that
// let submit call wg.Add without first checking the admission flag under
// the same mutex would let an Add slip in after Wait has already started,
// which under -race surfaces as "WaitGroup misuse: Add called concurrently
// with Wait" -- a process crash, not a normal assertion failure. The current
// implementation must let this converge cleanly instead.
func TestTaskConcurrentSubmitDuringCloseAdmissionAndStopDoesNotRace(t *testing.T) {
	logger, _ := taskCaptureLogger(t)
	var criticalCalled atomic.Bool
	rt := newTaskRuntime(logger, func(string) { criticalCalled.Store(true) })
	id := plugin.Identity{Plugin: "hammer"}

	const spammers = 50
	stop := make(chan struct{})
	var spammerWG sync.WaitGroup
	spammerWG.Add(spammers)
	for range spammers {
		go func() {
			defer spammerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					rt.submit(id, func(context.Context) {}, false)
				}
			}
		}()
	}

	deadline, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := rt.stopPlugin(id, deadline)

	close(stop)
	spammerWG.Wait()

	require.NoError(t, err, "准入关闭后 stopPlugin 必须能正常收敛，不受仍在敲门的并发提交影响")
	assert.False(t, criticalCalled.Load(), "普通任务被拒绝提交不应触发 critical")
}

// --- one task context per plugin instance ---------------------------------

// TestTaskCancelOnePluginDoesNotAffectAnothersRunningTask pins that
// stopPlugin cancels only its own plugin's task context. A shared
// process-wide context -- the design this package's own doc comment
// explicitly rejects -- would cancel B's task the moment A is stopped,
// which this test would catch by seeing bDone close before bRelease is
// closed.
func TestTaskCancelOnePluginDoesNotAffectAnothersRunningTask(t *testing.T) {
	logger, _ := taskCaptureLogger(t)
	rt := newTaskRuntime(logger, func(string) {})

	a := plugin.Identity{Plugin: "a"}
	b := plugin.Identity{Plugin: "b"}

	aCancelled := make(chan struct{})
	bStillRunning := make(chan struct{})
	bRelease := make(chan struct{})
	bDone := make(chan struct{})

	require.True(t, rt.submit(a, func(ctx context.Context) {
		<-ctx.Done()
		close(aCancelled)
	}, false))

	require.True(t, rt.submit(b, func(context.Context) {
		close(bStillRunning)
		<-bRelease
		close(bDone)
	}, false))

	<-bStillRunning // b's task has genuinely started before we touch a.

	deadline, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	require.NoError(t, rt.stopPlugin(a, deadline), "A 自己的任务应正常退出")
	<-aCancelled // guaranteed already closed: stopPlugin(a) only returns after A's wg.Wait does.

	select {
	case <-bDone:
		t.Fatal("停止插件 A 不应该让插件 B 仍在运行的任务提前结束——说明两者共享了同一个任务 context")
	default:
	}

	close(bRelease)
	require.NoError(t, rt.stopPlugin(b, deadline))
	<-bDone
}

// --- §5.5: shuttingDown must be a global flag, not a per-plugin one -------

// TestTaskShuttingDownFlagChangesWhetherReturnIsJudgedUnexpected pins §5.5's
// exact contract at the taskRuntime level: whether a critical task's
// unprompted return escalates depends solely on the runtime-global
// shuttingDown flag (set by closeAdmission), never on the per-plugin task
// context being cancelled.
//
// The second subtest is the one that matters: it reproduces exactly the
// scenario the design doc's §5.5 hazard describes -- a consumer plugin's own
// Stop makes its critical goroutine return while that plugin's own task
// context is deliberately still live (uncancelled). A per-plugin-context
// judgement would misread this as "nobody asked it to stop" and escalate; the
// global flag must not.
func TestTaskShuttingDownFlagChangesWhetherReturnIsJudgedUnexpected(t *testing.T) {
	logger, _ := taskCaptureLogger(t)

	t.Run("准入仍开放时的正常返回被判定为意外", func(t *testing.T) {
		var got []string
		var mu sync.Mutex
		rt := newTaskRuntime(logger, func(reason string) {
			mu.Lock()
			got = append(got, reason)
			mu.Unlock()
		})
		id := plugin.Identity{Plugin: "consumer-early"}

		require.True(t, rt.submit(id, func(context.Context) {
			// returns immediately: nobody cancelled it, nobody called closeAdmission.
		}, true))

		deadline, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		require.NoError(t, rt.stopPlugin(id, deadline))

		mu.Lock()
		defer mu.Unlock()
		require.Len(t, got, 1, "关机尚未开始时的正常返回，必须被判定为意外提前返回并触发一次 critical")
		assert.Contains(t, got[0], "意外提前返回")
	})

	t.Run("closeAdmission之后同一种返回不再被判定为意外", func(t *testing.T) {
		var got []string
		var mu sync.Mutex
		rt := newTaskRuntime(logger, func(reason string) {
			mu.Lock()
			got = append(got, reason)
			mu.Unlock()
		})
		id := plugin.Identity{Plugin: "consumer-during-stop"}

		release := make(chan struct{})
		require.True(t, rt.submit(id, func(ctx context.Context) {
			// Deliberately does NOT check ctx.Done() -- this plugin's own task
			// context is still live, exactly like a consumer whose Stop tells
			// it to stop pulling by other means (e.g. closing an internal
			// channel) before that plugin's context is ever cancelled.
			<-release
		}, true))

		// Mirrors doUnwind: shuttingDown flips true before any Stop runs.
		rt.closeAdmission()

		close(release) // the goroutine now returns -- "asked for", per the flag above.

		deadline, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		require.NoError(t, rt.stopPlugin(id, deadline))

		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, got, "shuttingDown 已置位后的正常返回不得被判定为意外，不能触发 critical")
	})
}

// --- spawnedCount -----------------------------------------------------------

// TestTaskSpawnedCountCountsEverAdmittedNotCurrentlyRunning pins the exact
// distinction App.assertLiveness depends on: spawnedCount must keep counting
// tasks that have already finished and been reaped, not report "how many are
// running right now". An implementation that decremented on completion (or
// derived the count from the WaitGroup) would read 0 here, right after the
// tasks it just admitted have all finished -- exactly the moment
// assertLiveness's "was anything ever admitted" question needs a true
// answer, not a "nothing is running this instant" one.
func TestTaskSpawnedCountCountsEverAdmittedNotCurrentlyRunning(t *testing.T) {
	logger, _ := taskCaptureLogger(t)
	rt := newTaskRuntime(logger, func(string) {})
	id := plugin.Identity{Plugin: "burst"}

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		require.True(t, rt.submit(id, func(context.Context) { wg.Done() }, false))
	}
	wg.Wait() // every admitted task has now actually finished running.

	deadline, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	require.NoError(t, rt.stopPlugin(id, deadline)) // ...and been reaped by Wait, too.

	assert.Equal(t, n, rt.spawnedCount(),
		"spawnedCount 必须统计曾经被准入的任务数，即便它们此刻已经全部跑完并被 Wait 收走")
}

// --- App-level wiring -------------------------------------------------------

// TestTaskContextTasksAcceptedReflectsAdmissionThroughHostAdapter pins that
// Context.TasksAccepted really reaches the runtime's admission flag through
// the production hostAdapter, not through some test-only shortcut. It reads
// TasksAccepted() twice on the very same *plugin.Context: once from Init
// (before shutdown begins, must be true) and once from inside this plugin's
// own Stop (which doUnwind only calls after closeAdmission has already run,
// must be false).
func TestTaskContextTasksAcceptedReflectsAdmissionThroughHostAdapter(t *testing.T) {
	var pc *plugin.Context
	var beforeShutdown, duringStop bool

	app := newTestApp(t, def("gate-watcher", func() plugin.Plugin {
		return &hooked{
			onInit: func(ctx *plugin.Context) error {
				pc = ctx
				beforeShutdown = ctx.TasksAccepted()
				return nil
			},
			onStop: func(context.Context) error {
				duringStop = pc.TasksAccepted()
				return nil
			},
		}
	}))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)

	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)
	assert.True(t, beforeShutdown,
		"关机开始之前，TasksAccepted 必须为 true——走的是 hostAdapter.TasksAccepted 的默认接通路径")
	assert.False(t, duringStop,
		"closeAdmission 在任何 Closer.Stop 之前执行，Stop 内部通过同一个 Context 看到的 TasksAccepted 必须已经是 false")
}

// TestTaskStopSpammingGoDuringShutdownDoesNotRaceOrPanic is the App-level
// twin of the concurrent-submit test above: a real plugin's Stop kicks off a
// background goroutine that hammers ctx.Go thousands of times while doUnwind
// moves on to cancel-and-wait that very plugin's task group. Because
// closeAdmission runs before any Stop, every one of those calls must be
// rejected (no Add can occur), so the cancel-and-wait that follows has
// nothing to race against -- and the whole run must still exit cleanly.
func TestTaskStopSpammingGoDuringShutdownDoesNotRaceOrPanic(t *testing.T) {
	extra, cap := captureLogs(t)

	const spamCount = 3000
	var pc *plugin.Context
	spamDone := make(chan struct{})

	app := newTestApp(t, def("spammer", func() plugin.Plugin {
		return &hooked{
			onInit: func(ctx *plugin.Context) error { pc = ctx; return nil },
			onStop: func(context.Context) error {
				// Fire-and-forget: Stop itself returns immediately, letting
				// doUnwind proceed to stopPlugin (cancel + wg.Wait) for this
				// plugin's group while this goroutine is still actively
				// calling ctx.Go -- the exact overlap §5.4 is designed to
				// make harmless.
				go func() {
					defer close(spamDone)
					for range spamCount {
						pc.Go(func(context.Context) {})
					}
				}()
				return nil
			},
		}
	}))

	done := runAsync(app, writeConfig(t, extra+"xbc:\n  shutdown_timeout: 500ms\n")...)
	awaitReady(t, app)

	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	waitFor(t, "Stop 内后台的 spam goroutine 应该跑完", func() bool {
		select {
		case <-spamDone:
			return true
		default:
			return false
		}
	})

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code, "关机期间被拒绝的海量并发提交不应升级为失败退出，也不应引发崩溃")
	assert.True(t, cap.containsMessage(t, "任务组已关闭"),
		"这些并发提交必须真的走到了拒绝路径并留下日志，而不是恰好全部发生在 closeAdmission 之前")
}

// --- §5.5's critical escalation paths (App-level, for the exit code) ------

// TestTaskGoCriticalPanicEscalatesShutdownWithNonZeroExit pins the first half
// of §5.5's escalation contract: a GoCritical goroutine's panic must be
// recovered (the process must not simply crash), logged, and still treated
// as a failure that ends the whole run with exit code 1 -- "recovered" must
// not silently become "ignored".
func TestTaskGoCriticalPanicEscalatesShutdownWithNonZeroExit(t *testing.T) {
	extra, cap := captureLogs(t)

	app := newTestApp(t, def("critical-panicker", func() plugin.Plugin {
		return &hooked{
			onStart: func(ctx *plugin.Context) error {
				ctx.GoCritical(func(context.Context) { panic("boom") })
				return nil
			},
		}
	}))

	done := runAsync(app, writeConfig(t, extra+"xbc:\n  shutdown_timeout: 500ms\n")...)
	res := awaitResult(t, done)

	assert.Equal(t, 1, res.code, "GoCritical 的 goroutine panic 必须让整个运行以退出码 1 结束")
	assert.True(t, cap.containsMessage(t, "托管 goroutine panic"), "panic 必须被 recover 并记录日志")
	assert.True(t, cap.containsMessage(t, "收到 critical 信号"), "panic 之后必须真的走了 critical 升级路径")
}

// TestTaskGoCriticalUnpromptedReturnEscalatesShutdownWithNonZeroExit pins
// the second half: a GoCritical goroutine that simply returns, with nobody
// having asked it to (no closeAdmission, no cancelled context), must be
// treated exactly like a panic -- exit code 1 -- because a consumer loop
// that silently stopped consuming is a failure even though the process
// is still up.
func TestTaskGoCriticalUnpromptedReturnEscalatesShutdownWithNonZeroExit(t *testing.T) {
	extra, cap := captureLogs(t)

	app := newTestApp(t, def("critical-quitter", func() plugin.Plugin {
		return &hooked{
			onStart: func(ctx *plugin.Context) error {
				ctx.GoCritical(func(context.Context) {
					// returns immediately: nothing requested this.
				})
				return nil
			},
		}
	}))

	done := runAsync(app, writeConfig(t, extra+"xbc:\n  shutdown_timeout: 500ms\n")...)
	res := awaitResult(t, done)

	assert.Equal(t, 1, res.code,
		"GoCritical 的 goroutine 在无人要求的情况下正常返回，必须视为失败，退出码为 1")

	// "意外提前返回" travels as the reason argument of the critical-escalation
	// log line, not as its message text -- unlike the panic case above, there
	// is no separate log call at the point of the return itself.
	var sawReason bool
	for _, e := range cap.entries(t) {
		if strings.Contains(e.Reason, "意外提前返回") {
			sawReason = true
		}
	}
	assert.True(t, sawReason, "critical 升级的 reason 字段必须写明 goroutine 是意外提前返回，而不是别的原因")
	assert.True(t, cap.containsMessage(t, "收到 critical 信号"))
}

// TestTaskPlainGoPanicIsLoggedButDoesNotEscalate is the control case for the
// two tests above: a panic inside a plain (non-critical) Go task must only
// be logged, and the application must keep running until it is explicitly
// asked to stop. Asserting "no critical log appeared" only means something
// once we have deterministically observed that the panic itself really did
// happen first -- otherwise a broken implementation that swallowed the
// panic message entirely would pass this test for the wrong reason.
func TestTaskPlainGoPanicIsLoggedButDoesNotEscalate(t *testing.T) {
	extra, cap := captureLogs(t)

	app := newTestApp(t, def("go-panicker", func() plugin.Plugin {
		return &hooked{
			onStart: func(ctx *plugin.Context) error {
				ctx.Go(func(context.Context) { panic("harmless") })
				return nil
			},
		}
	}))

	done := runAsync(app, writeConfig(t, extra+"xbc:\n  shutdown_timeout: 500ms\n")...)
	awaitReady(t, app)

	waitFor(t, "普通 Go 任务的 panic 应该已经被记录", func() bool {
		return cap.containsMessage(t, "托管 goroutine panic")
	})
	assert.False(t, cap.containsMessage(t, "收到 critical 信号"), "普通 Go 的 panic 不得升级为 critical")

	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code, "普通 Go 的 panic 之后应用应继续运行，直到被明确要求停止")
}

// --- drainRemaining ----------------------------------------------------------

// TestTaskDrainRemainingReapsGoroutineFromPluginWhoseInitFailed pins the
// exact scenario drainRemaining's own doc comment describes: a plugin
// submits a managed task from inside Init and then fails Init itself. That
// plugin never reaches assembly.Container.Initialized() (MarkInitialized only runs
// after Init succeeds), so doUnwind's per-plugin walk never visits its task
// group -- only drainRemaining, called once after that walk, can still reach
// it. If drainRemaining forgot such a group, the goroutine below would never
// observe cancellation and this test would time out waiting on cancelled.
func TestTaskDrainRemainingReapsGoroutineFromPluginWhoseInitFailed(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})

	app := newTestApp(t, def("half-initialized", func() plugin.Plugin {
		return &initOnly{onInit: func(ctx *plugin.Context) error {
			ctx.Go(func(taskCtx context.Context) {
				close(started)
				<-taskCtx.Done()
				close(cancelled)
			})
			return errors.New("模拟 Init 中途失败")
		}}
	}))

	done := runAsync(app, quietConfig(t, "")...)
	<-started // the task was genuinely admitted before Init returned its error.

	res := awaitResult(t, done)
	assert.Equal(t, 1, res.code, "Init 失败必须以非零退出码结束")

	select {
	case <-cancelled:
	default:
		t.Fatal("drainRemaining 必须取消并等待 Init 失败前已提交的托管任务，不能遗漏——" +
			"该任务的 goroutine 从未观察到取消")
	}
}
