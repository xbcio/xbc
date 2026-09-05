package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

// runtimeTestConfigWith writes the quiet base configuration plus extra
// top-level sections, for the cases whose subject is the configuration file
// rather than the lifecycle.
func runtimeTestConfigWith(t *testing.T, shutdownTimeout time.Duration, extra string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: " +
		shutdownTimeout.String() + "\n" + extra
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return []string{"--config", path}
}

func TestNewComposesExplicitBundlesInsteadOfTheProcessCatalog(t *testing.T) {
	declared := plugin.Define("explicitly-composed", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	other := plugin.Define("appended-later", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app, err := New(WithBundles(plugin.BundleOf(declared)), WithBundles(plugin.BundleOf(other)))
	require.NoError(t, err)
	require.Len(t, app.bundles, 2, "repeated WithBundles appends to one composition")

	code, err := app.Execute(context.Background(), append([]string{"doctor"}, runtimeTestConfig(t, time.Second)...))
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, 2, app.plan.DefinitionCount(),
		"an explicitly composed App sees exactly its own Bundles")
}

func TestNewWithoutBundlesFreezesTheProcessCatalog(t *testing.T) {
	app, err := New()
	require.NoError(t, err)
	require.Len(t, app.bundles, 1)
	assert.Equal(t, autoload.Freeze(), app.bundles[0],
		"the implicit composition is the frozen autoload catalog, not a fresh one")
}

func TestNewReportsAFailingOptionByPosition(t *testing.T) {
	failing := errors.New("option refused")
	_, err := New(WithBundles(), func(*appOptions) error { return failing })
	assert.ErrorIs(t, err, failing)

	_, err = New(WithBundles(), nil)
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: option 1 is nil")

	_, err = New(nil)
	assert.EqualError(t, err, "xbc: option 0 is nil")
}

func TestOptionErrorRendersItsIndexWithoutStrconv(t *testing.T) {
	for index, want := range map[int]string{0: "xbc: option 0 is nil", 7: "xbc: option 7 is nil", 1024: "xbc: option 1024 is nil"} {
		assert.EqualError(t, optionError(index, "is nil"), want)
	}
}

func TestEmptyCompositionAndFullyDisabledCompositionFailDifferently(t *testing.T) {
	empty := newRuntimeTestApp()
	code, err := empty.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no plugin was declared, nothing to do")

	definition := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-off")})
	disabled := newRuntimeTestApp(definition)
	code, err = disabled.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared 1 plugins, but none were enabled; disabled: switched-off")
}

func TestDoctorSucceedsEvenWhenNothingIsEnabled(t *testing.T) {
	definition := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-off")})

	for _, app := range []*App{newRuntimeTestApp(), newRuntimeTestApp(definition)} {
		code, err := app.Execute(context.Background(), append([]string{"doctor"}, runtimeTestConfig(t, time.Second)...))
		require.NoError(t, err, "doctor diagnoses a composition instead of refusing to run on it")
		assert.Equal(t, 0, code)
	}
}

func TestStartupRequiresALongLivedCapability(t *testing.T) {
	value := func(plugin.BuildContext) (*runtimeTestValue, error) { return &runtimeTestValue{}, nil }

	inert := newRuntimeTestApp(plugin.Define("inert", value, plugin.Options[*runtimeTestValue]{
		Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
			Init: func(*runtimeTestValue, *plugin.Context) error { return nil },
		},
	}))
	code, err := inert.Execute(context.Background(), runtimeTestConfig(t, time.Second))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: no plugin provides a long-lived capability; enabled plugins: inert")

	for name, lifecycle := range map[string]plugin.Lifecycle[*runtimeTestValue]{
		"traffic opener": {
			OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		},
		"managed task": {
			Start: func(_ *runtimeTestValue, ctx *plugin.Context) error {
				ctx.Go(func(taskContext context.Context) { <-taskContext.Done() })
				return nil
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := newRuntimeTestApp(plugin.Define("alive", value, plugin.Options[*runtimeTestValue]{Lifecycle: lifecycle}))
			result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
			awaitRuntimeTestReady(t, app)
			app.requestStop(stopReasonSignal)
			completed := awaitRuntimeTestResult(t, result)
			require.NoError(t, completed.err)
			assert.Equal(t, 0, completed.code)
		})
	}
}

func TestOrphanConfigurationFailsStartupWhileADisabledSectionDoesNot(t *testing.T) {
	definition := plugin.Define("known", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
	}})

	orphan := newRuntimeTestApp(definition)
	code, err := orphan.Execute(context.Background(),
		runtimeTestConfigWith(t, time.Second, "plugins:\n  knwon:\n    enabled: true\n"))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugins.knwon has configuration but no corresponding plugin")

	disabled := newRuntimeTestApp(definition)
	code, err = disabled.Execute(context.Background(),
		runtimeTestConfigWith(t, time.Second, "plugins:\n  known:\n    enabled: false\n"))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none were enabled",
		"a declared but disabled section is a disablement, never an orphan")
}

func TestTwoAppsInOneProcessOwnIndependentInstancesAndState(t *testing.T) {
	// A distinct, non-empty type: Go may give every pointer to a zero-sized
	// value the same address, which would make the isolation assertion below
	// pass or fail for reasons unrelated to App ownership.
	type isolatedValue struct{ marker int }

	contexts := make(chan *plugin.Context, 2)
	values := make(chan *isolatedValue, 2)
	definition := plugin.Define("isolated", func(plugin.BuildContext) (*isolatedValue, error) {
		value := &isolatedValue{marker: 1}
		values <- value
		return value, nil
	}, plugin.Options[*isolatedValue]{Lifecycle: plugin.Lifecycle[*isolatedValue]{
		Start: func(_ *isolatedValue, ctx *plugin.Context) error {
			contexts <- ctx
			return nil
		},
		OpenTraffic: func(*isolatedValue, *plugin.Context) error { return nil },
	}})

	first, second := newRuntimeTestApp(definition), newRuntimeTestApp(definition)
	firstResult := executeRuntimeTest(first, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, first)
	secondResult := executeRuntimeTest(second, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, second)

	firstValue, secondValue := <-values, <-values
	assert.NotSame(t, firstValue, secondValue, "each App constructs its own plugin value")
	firstContext, secondContext := <-contexts, <-contexts
	assert.NotSame(t, firstContext, secondContext, "each App owns its own lifecycle Context")

	first.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, firstResult).err)
	assert.NoError(t, secondContext.Err(), "one App's shutdown cannot cancel another App's Context")

	second.requestStop(stopReasonSignal)
	require.NoError(t, awaitRuntimeTestResult(t, secondResult).err)
}
