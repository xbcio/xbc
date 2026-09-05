package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func TestExecuteRejectsANilContextAndASecondExecution(t *testing.T) {
	app := newRuntimeTestApp()
	code, err := app.Execute(nil, nil) //nolint:staticcheck // a nil context is exactly the rejected input
	assert.Equal(t, 1, code)
	require.EqualError(t, err, "xbc: Execute context cannot be nil")

	code, err = app.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code, "the empty composition still fails, but Execute was consumed")
	require.Error(t, err)

	code, err = app.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.EqualError(t, err, "xbc: App.Execute can only be called once")
}

func TestExecuteRefusesAnAlreadyCanceledCallerContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := newRuntimeTestApp().Execute(canceled, runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "context was canceled before Execute")
}

func TestCallerCancellationStopsTheRunWithoutOwningTheProcess(t *testing.T) {
	definition := plugin.Define("cancelable", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	app := newRuntimeTestApp(definition)

	caller, cancel := context.WithCancel(context.Background())
	result := make(chan runtimeTestResult, 1)
	go func() {
		code, err := app.Execute(caller, runtimeTestConfig(t, time.Second))
		result <- runtimeTestResult{code: code, err: err}
	}()
	awaitRuntimeTestReady(t, app)
	cancel()

	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err, "caller cancellation is a clean stop, not a failure")
	assert.Equal(t, 0, completed.code)
	assert.Equal(t, stopReasonContext, app.currentStopReason())
}

func TestUsageErrorsExitTwoWhileRuntimeFailuresExitOne(t *testing.T) {
	for name, testCase := range map[string]struct {
		args []string
		code int
	}{
		"unknown subcommand": {args: []string{"serve"}, code: 2},
		"unknown flag":       {args: []string{"--nope"}, code: 2},
		"stray argument":     {args: []string{"--", "leftover"}, code: 2},
		"assembly failure":   {args: nil, code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			args := append(append([]string(nil), testCase.args...), runtimeTestConfig(t, time.Second)...)
			code, err := newRuntimeTestApp().Execute(context.Background(), args)
			require.Error(t, err)
			assert.Equal(t, testCase.code, code)
		})
	}
}

func TestDoctorNeverInvokesALifecycleHook(t *testing.T) {
	var hooks atomic.Int32
	count := func(*runtimeTestValue, *plugin.Context) error {
		hooks.Add(1)
		return nil
	}
	definition := plugin.Define("doctor-inert", func(plugin.BuildContext) (*runtimeTestValue, error) {
		hooks.Add(1)
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Init: count, Migrate: count, Start: count, OpenTraffic: count,
		Stop: func(*runtimeTestValue, context.Context) error {
			hooks.Add(1)
			return nil
		},
	}})

	app := newRuntimeTestApp(definition)
	args := append([]string{"doctor", "--migrate"}, runtimeTestConfig(t, time.Second)...)
	code, err := app.Execute(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Zero(t, hooks.Load(), "doctor reports the plan and stops before construction")
	assert.Nil(t, app.owned)
}

func TestMigrationRunsOnceAndUnwindsWithoutStartingTraffic(t *testing.T) {
	for name, args := range map[string][]string{
		"subcommand": {"migrate"},
		"flag":       {"--migrate"},
	} {
		t.Run(name, func(t *testing.T) {
			var stages []string
			definition := plugin.Define("migrating", func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
				Migrate:     func(*runtimeTestValue, *plugin.Context) error { stages = append(stages, "migrate"); return nil },
				Start:       func(*runtimeTestValue, *plugin.Context) error { stages = append(stages, "start"); return nil },
				OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { stages = append(stages, "open"); return nil },
				Stop: func(*runtimeTestValue, context.Context) error {
					stages = append(stages, "stop")
					return nil
				},
			}})
			app := newRuntimeTestApp(definition)
			full := append(append([]string(nil), args...), runtimeTestConfig(t, time.Second)...)

			if name == "subcommand" {
				code, err := app.Execute(context.Background(), full)
				require.NoError(t, err)
				assert.Equal(t, 0, code)
				assert.Equal(t, []string{"migrate", "stop"}, stages,
					"the migrate subcommand stops after migrating instead of serving")
				assertChannelOpen(t, app.trafficGate, "the migrate subcommand released the traffic gate")
				return
			}

			result := executeRuntimeTest(app, full...)
			awaitRuntimeTestReady(t, app)
			app.requestStop(stopReasonSignal)
			completed := awaitRuntimeTestResult(t, result)
			require.NoError(t, completed.err)
			assert.Equal(t, []string{"migrate", "start", "open", "stop"}, stages,
				"--migrate migrates and then boots normally")
		})
	}
}

func TestAutoMigrateConfigurationMigratesWithoutAnyFlag(t *testing.T) {
	var migrated atomic.Int32
	definition := plugin.Define("auto-migrating", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Migrate: func(*runtimeTestValue, *plugin.Context) error {
			migrated.Add(1)
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	app := newRuntimeTestApp(definition)
	args := []string{"--config", writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: 1s\n  auto_migrate: true\n")}

	result := executeRuntimeTest(app, args...)
	awaitRuntimeTestReady(t, app)
	app.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, result).err)
	assert.Equal(t, int32(1), migrated.Load())
}

func TestLifecycleContextForwardsTheCallerScopeAndTheHostIdentity(t *testing.T) {
	type keyType struct{}
	key := keyType{}
	deadline := time.Now().Add(time.Minute)
	caller, cancel := context.WithDeadline(context.WithValue(context.Background(), key, "caller-value"), deadline)
	defer cancel()

	observed := make(chan *plugin.Context, 1)
	definition := plugin.Define("context-aware", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{
		Instances: plugin.MultipleInstances,
		Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
			Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
				observed <- ctx
				return nil
			},
			OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		},
	})
	app := newRuntimeTestApp(definition)
	args := []string{"--config", writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: 1s\nplugins:\n  context-aware:\n    readonly: {}\n")}

	result := make(chan runtimeTestResult, 1)
	go func() {
		code, err := app.Execute(caller, args)
		result <- runtimeTestResult{code: code, err: err}
	}()
	awaitRuntimeTestReady(t, app)

	ctx := <-observed
	assert.Equal(t, plugin.Identity{Plugin: "context-aware", Instance: "readonly"}, ctx.Identity(),
		"the host names the exact instance, not just the key")
	got, ok := ctx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, deadline, got, time.Millisecond)
	assert.Equal(t, "caller-value", ctx.Value(key))
	require.NotNil(t, ctx.Log())

	assert.True(t, ctx.RequestShutdown("listener\n  failed"))
	assert.Equal(t, "plugin:context-aware[readonly]:listener failed", app.currentStopReason(),
		"the host normalizes whitespace and attributes the reason to its plugin")
	assert.False(t, ctx.RequestShutdown(""), "only the first request wins")

	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the execution scope is canceled by the stop request")
}

func TestHostAdapterDegradesWhenTheAppHasNoRuntimeStateYet(t *testing.T) {
	app := newApp(nil)
	host := hostAdapter{app: app}
	assert.Equal(t, context.Background(), host.ExecutionContext(),
		"a not-yet-executing App exposes Background rather than a nil context")
	assert.NotNil(t, host.Logger())
	assert.NotNil(t, host.TrafficGate())
	assert.False(t, host.SubmitTask(plugin.Identity{Plugin: "p"}, func(context.Context) {}, false),
		"there is no task runtime before bootstrap")

	app.logger = nil
	assert.NotNil(t, app.log(), "log falls back to the process logger")
}

func TestCriticalTaskFailureStopsTheRunWithANonZeroExitCode(t *testing.T) {
	failure := errors.New("critical exploded")
	definition := plugin.Define("critical-failure", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			started := make(chan struct{})
			require.True(t, ctx.GoCritical(func(context.Context) {
				close(started)
				panic(failure)
			}))
			<-started
			return nil
		},
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})
	app := newRuntimeTestApp(definition)

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	completed := awaitRuntimeTestResult(t, result)
	assert.Equal(t, 1, completed.code)
	require.Error(t, completed.err)
	assert.Contains(t, completed.err.Error(), "critical exploded")
}
