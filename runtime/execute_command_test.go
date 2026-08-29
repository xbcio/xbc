package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// argument parsing itself lives in package cli and is tested there. What stays
// here is the half that only exists once a real App is driving: which
// subcommand reaches which lifecycle stages, and which exit code each
// outcome leaves behind. Those run through App.Execute, not through the parser.

// --- doctor subcommand ----------------------------------------------------

// TestCommandDoctorNeverCallsLifecycleHooks pins requirement #3: doctor
// reports what would be assembled and returns without ever driving a single
// lifecycle stage. An enabled plugin with every hook wired up is used
// precisely so that "doctor happens to skip lifecycle stages because
// nothing was enabled" cannot be mistaken for the real guarantee.
func TestCommandDoctorNeverCallsLifecycleHooks(t *testing.T) {
	var initCalled, migrateCalled, startCalled, openCalled bool

	app := newTestApp(t, def("command-echo", func() plugin.Plugin {
		return &hooked{
			onInit:    func(*plugin.Context) error { initCalled = true; return nil },
			onMigrate: func(*plugin.Context) error { migrateCalled = true; return nil },
			onStart:   func(*plugin.Context) error { startCalled = true; return nil },
			onOpen:    func(*plugin.Context) error { openCalled = true; return nil },
		}
	}))

	// "doctor" must be the first argument: cli.ParseArgs only recognizes a
	// subcommand as the first non-flag token.
	ch := runAsync(app, append([]string{"doctor"}, quietConfig(t, "")...)...)
	res := awaitResult(t, ch)

	require.NoError(t, res.err, "doctor 只报告现状，不应该失败")
	assert.Equal(t, 0, res.code)
	assert.False(t, initCalled, "doctor 不应该调用任何插件的 Init")
	assert.False(t, migrateCalled, "doctor 不应该调用任何插件的 Migrate")
	assert.False(t, startCalled, "doctor 不应该调用任何插件的 Start")
	assert.False(t, openCalled, "doctor 不应该调用任何插件的 OpenTraffic")
}

// TestCommandDoctorSucceedsOnEmptyCatalog pins the first half of "doctor 在
// catalog 为空或无插件启用时也不报错": a catalog with nothing declared at
// all must still let doctor report (an empty report) and exit 0, precisely
// because doctor's own check in App.run happens before the "nothing
// enabled" failure that a normal boot would hit.
func TestCommandDoctorSucceedsOnEmptyCatalog(t *testing.T) {
	app := newTestApp(t) // no Definitions at all

	ch := runAsync(app, append([]string{"doctor"}, quietConfig(t, "")...)...)
	res := awaitResult(t, ch)

	require.NoError(t, res.err, "空 catalog 下 doctor 仍然应该成功返回")
	assert.Equal(t, 0, res.code)
}

// TestCommandDoctorSucceedsWhenNoPluginEnabled pins the second half: a
// catalog that declares a plugin, but whose Activation is gated on a config
// section this run never provides, must also leave doctor's report clean --
// "declared but disabled" is doctor's own job to surface, not a failure
// doctor itself should suffer from.
func TestCommandDoctorSucceedsWhenNoPluginEnabled(t *testing.T) {
	disabled := plugin.Definition{
		Key:        "command-disabled",
		Factory:    func() plugin.Plugin { return new(bare) },
		Activation: plugin.Configured("command-disabled-section"),
	}
	app := newTestApp(t, disabled)

	ch := runAsync(app, append([]string{"doctor"}, quietConfig(t, "")...)...)
	res := awaitResult(t, ch)

	require.NoError(t, res.err, "全部插件都未启用时 doctor 仍然应该成功返回")
	assert.Equal(t, 0, res.code)
}

// --- migrate subcommand ----------------------------------------------------

// TestCommandMigrateSubcommandRunsOnceAndUnwinds pins requirement #4: the
// "migrate" subcommand is a one-shot run. Migrate must be called, the run
// must still unwind in full afterwards (Stop called, per doUnwind's own
// reasoning that a Migrator's Init may have opened a resource "the process
// is about to exit anyway" does not excuse leaving open), and Start /
// OpenTraffic must never run -- migrate exits before either stage.
func TestCommandMigrateSubcommandRunsOnceAndUnwinds(t *testing.T) {
	var migrateCalled, startCalled, openCalled, stopCalled bool
	var lifecycleCtx *plugin.Context
	var lifecycleDoneBeforeStop bool
	var lifecycleErrInStop error

	app := newTestApp(t, def("command-mig", func() plugin.Plugin {
		return &hooked{
			onInit:    func(ctx *plugin.Context) error { lifecycleCtx = ctx; return nil },
			onMigrate: func(*plugin.Context) error { migrateCalled = true; return nil },
			onStart:   func(*plugin.Context) error { startCalled = true; return nil },
			onOpen:    func(*plugin.Context) error { openCalled = true; return nil },
			onStop: func(context.Context) error {
				stopCalled = true
				lifecycleErrInStop = lifecycleCtx.Err()
				select {
				case <-lifecycleCtx.Done():
					lifecycleDoneBeforeStop = true
				default:
				}
				return nil
			},
		}
	}))

	ch := runAsync(app, append([]string{"migrate"}, quietConfig(t, "")...)...)
	res := awaitResult(t, ch)

	require.NoError(t, res.err, "migrate 子命令跑完不应该返回错误")
	assert.Equal(t, 0, res.code)
	assert.True(t, migrateCalled, "migrate 子命令必须调用 Migrate")
	assert.True(t, stopCalled, "一次性 migrate 运行结束后仍必须完整 unwind（调用 Stop）")
	assert.False(t, startCalled, "migrate 子命令绝不能调用 Start")
	assert.False(t, openCalled, "migrate 子命令绝不能调用 OpenTraffic")
	assert.True(t, lifecycleDoneBeforeStop,
		"migrate 成功后的 completed unwind 必须先关闭 plugin.Context.Done 再调用 Stop")
	assert.ErrorIs(t, lifecycleErrInStop, context.Canceled)
	assert.Equal(t, stopReasonCompleted, app.stopReason)
}

// --- exit code conventions --------------------------------------------------

// TestCommandExitCodeUsageErrorIsTwo pins that a command-line usage error
// (an undeclared flag) leaves the process exiting 2, the flag package's own
// convention, distinct from a runtime/assembly failure.
func TestCommandExitCodeUsageErrorIsTwo(t *testing.T) {
	app := newTestApp(t)

	code, err := app.Execute(context.Background(), []string{"--this-flag-does-not-exist"})
	require.Error(t, err)
	assert.Equal(t, 2, code, "命令行用法错误必须返回退出码 2")
}

// TestCommandExitCodeUnknownSubcommandIsTwo pins the same convention for an
// unrecognized subcommand -- it is a usage error, not an assembly failure.
func TestCommandExitCodeUnknownSubcommandIsTwo(t *testing.T) {
	app := newTestApp(t)

	code, err := app.Execute(context.Background(), []string{"frobnicate"})
	require.Error(t, err)
	assert.Equal(t, 2, code, "未知子命令必须返回退出码 2")
}

// TestCommandExitCodeAssemblyFailureIsOne pins that a run which parses
// cleanly but fails before ever reaching the wait loop -- here, a catalog
// with nothing declared, so nothing is ever enabled -- leaves the process
// exiting 1, not 2 and not 0.
func TestCommandExitCodeAssemblyFailureIsOne(t *testing.T) {
	app := newTestApp(t) // nothing declared: assembly produces zero instances

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	require.Error(t, err)
	assert.Equal(t, 1, code, "装配/运行失败必须返回退出码 1")
}

// TestCommandExitCodeCleanStopIsZero pins the normal case: a boot that
// starts, is asked to stop, and unwinds cleanly leaves the process exiting
// 0.
func TestCommandExitCodeCleanStopIsZero(t *testing.T) {
	app := newTestApp(t, liveness("command-runner"))

	ch := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, ch)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code, "正常收到停止请求并干净关机必须返回退出码 0")
}

// TestCommandShutdownTimeoutNonPositiveFailsAtLoadSettings pins requirement
// #6: xbc.shutdown_timeout must be positive, and a non-positive value is
// rejected at loadSettings -- before a single plugin is touched -- rather
// than being allowed through to silently make every shutdown budget-free.
func TestCommandShutdownTimeoutNonPositiveFailsAtLoadSettings(t *testing.T) {
	for _, v := range []string{"0s", "-1s"} {
		t.Run(v, func(t *testing.T) {
			app := newTestApp(t, liveness("command-runner"))
			args := writeConfig(t, "log:\n  console:\n    enabled: false\n"+
				"xbc:\n  shutdown_timeout: "+v+"\n")

			code, err := app.Execute(context.Background(), args)
			require.Error(t, err)
			assert.Equal(t, 1, code, "非正数的 shutdown_timeout 必须在装配阶段失败，退出码 1")
			assert.Contains(t, err.Error(), "shutdown_timeout")
		})
	}
}
