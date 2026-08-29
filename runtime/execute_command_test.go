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

	require.NoError(t, res.err, "doctor should only report the current state and should not fail")
	assert.Equal(t, 0, res.code)
	assert.False(t, initCalled, "doctor should not call any plugin's Init")
	assert.False(t, migrateCalled, "doctor should not call any plugin's Migrate")
	assert.False(t, startCalled, "doctor should not call any plugin's Start")
	assert.False(t, openCalled, "doctor should not call any plugin's OpenTraffic")
}

// TestCommandDoctorSucceedsOnEmptyCatalog pins the first half of "doctor does not
// report an error when the catalog is empty or no plugins are enabled": a catalog with nothing declared at
// all must still let doctor report (an empty report) and exit 0, precisely
// because doctor's own check in App.run happens before the "nothing
// enabled" failure that a normal boot would hit.
func TestCommandDoctorSucceedsOnEmptyCatalog(t *testing.T) {
	app := newTestApp(t) // no Definitions at all

	ch := runAsync(app, append([]string{"doctor"}, quietConfig(t, "")...)...)
	res := awaitResult(t, ch)

	require.NoError(t, res.err, "doctor should still succeed in an empty catalog")
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

	require.NoError(t, res.err, "doctor should still succeed when all plugins are disabled")
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

	require.NoError(t, res.err, "The migrate subcommand should not return an error after completing")
	assert.Equal(t, 0, res.code)
	assert.True(t, migrateCalled, "The migrate subcommand must call Migrate")
	assert.True(t, stopCalled, "After a single migrate run, the complete unwind (calling Stop) must still be performed")
	assert.False(t, startCalled, "The migrate subcommand must never call Start")
	assert.False(t, openCalled, "The migrate subcommand must never call OpenTraffic")
	assert.True(t, lifecycleDoneBeforeStop,
		"After a successful migrate, the completed unwind must first close plugin.Context.Done before calling Stop")
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
	assert.Equal(t, 2, code, "Command line usage errors must return exit code 2")
}

// TestCommandExitCodeUnknownSubcommandIsTwo pins the same convention for an
// unrecognized subcommand -- it is a usage error, not an assembly failure.
func TestCommandExitCodeUnknownSubcommandIsTwo(t *testing.T) {
	app := newTestApp(t)

	code, err := app.Execute(context.Background(), []string{"frobnicate"})
	require.Error(t, err)
	assert.Equal(t, 2, code, "Unknown subcommand must return exit code 2")
}

// TestCommandExitCodeAssemblyFailureIsOne pins that a run which parses
// cleanly but fails before ever reaching the wait loop -- here, a catalog
// with nothing declared, so nothing is ever enabled -- leaves the process
// exiting 1, not 2 and not 0.
func TestCommandExitCodeAssemblyFailureIsOne(t *testing.T) {
	app := newTestApp(t) // nothing declared: assembly produces zero instances

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	require.Error(t, err)
	assert.Equal(t, 1, code, "Assembly or runtime failure must return exit code 1")
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
	assert.Equal(t, 0, res.code, "Normal receipt of a stop request and clean shutdown must return exit code 0")
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
			assert.Equal(t, 1, code, "A non-positive shutdown_timeout must fail during assembly phase with exit code 1")
			assert.Contains(t, err.Error(), "shutdown_timeout")
		})
	}
}
