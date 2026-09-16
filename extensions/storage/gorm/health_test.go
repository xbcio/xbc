package gorm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlib "gorm.io/gorm"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestHealthDefinitionIsDistinctFromTheDatabaseDefinition(t *testing.T) {
	var zero plugin.Definition
	require.NotEqual(t, zero, healthDefinition)
	assert.NotEqual(t, definition, healthDefinition, "the readiness probe must be its own Definition")
	assert.NotEqual(t, Key, HealthKey, "the readiness probe must have its own Key")
}

func TestHealthChecksReportOneReadinessCheckPerInstance(t *testing.T) {
	db, err := openDatabase(plugin.BuildContext{}, sqliteConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopDatabase(db, context.Background()) })

	probe, err := newHealthProbe([]plugin.Entry[*gormlib.DB]{
		{Identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, Value: db},
		{Identity: plugin.Identity{Plugin: Key, Instance: "readonly"}, Value: db},
	})
	require.NoError(t, err)

	checks := probe.HealthChecks()
	require.Len(t, checks, 2)
	assert.Empty(t, checks[0].Name, "the default instance contributes an unqualified check")
	assert.Equal(t, "readonly", checks[1].Name)
	for _, check := range checks {
		assert.Equal(t, health.Readiness, check.Kind)
		assert.Zero(t, check.Timeout, "checks inherit the plugins.health timeout")
	}

	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	assert.True(t, report.Healthy(), "an open pool must report up: %+v", report)
}

func TestHealthChecksReportDownAfterThePoolIsClosed(t *testing.T) {
	db, err := openDatabase(plugin.BuildContext{}, sqliteConfig(t))
	require.NoError(t, err)

	probe, err := newHealthProbe([]plugin.Entry[*gormlib.DB]{
		{Identity: plugin.Identity{Plugin: Key, Instance: "readonly"}, Value: db},
	})
	require.NoError(t, err)
	require.NoError(t, stopDatabase(db, context.Background()))

	report := health.Check(context.Background(), health.Readiness, probe.HealthChecks(), time.Second)
	assert.Equal(t, health.Down, report.Status)
	require.Len(t, report.Checks, 1)
	require.Error(t, report.Checks[0].Error)
	assert.Contains(t, report.Checks[0].Error.Error(), "ping")
}

func TestHealthProbeRejectsANilDatabase(t *testing.T) {
	_, err := newHealthProbe([]plugin.Entry[*gormlib.DB]{
		{Identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}},
	})
	require.Error(t, err, "a nil database must fail construction instead of panicking during a probe")
}

// TestAssembledHealthPluginReportsEveryConfiguredDatabase is the load-bearing
// test for this Definition's existence. The probe exists because *gorm.DB, a
// third-party type, cannot implement health.Contributor: assembly rejects a
// contract its declaring Definition's primary type is not assignable to. This
// test proves the second-Definition shape actually reaches the aggregator, and
// that a configured instance appears in the readiness report without any change
// at the composition root.
func TestAssembledHealthPluginReportsEveryConfiguredDatabase(t *testing.T) {
	environment := testEnvironment(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"primary": map[string]any{
					"driver": "sqlite",
					"dsn":    "file:aggregated-primary?mode=memory&cache=shared",
				},
				"readonly": map[string]any{
					"driver": "sqlite",
					"dsn":    "file:aggregated-readonly?mode=memory&cache=shared",
				},
			},
		},
	})

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), health.Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(testRuntimeHost{execution: context.Background()}, identity)
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, instance := range []plugin.Identity{
			{Plugin: Key, Instance: "primary"},
			{Plugin: Key, Instance: "readonly"},
		} {
			if constructedInstance, ok := constructed.Instance(instance); ok {
				_ = constructedInstance.StopBounded(context.Background(), time.Second)
			}
		}
	})

	aggregator, ok := constructed.Instance(plugin.Identity{Plugin: health.Key, Instance: plugin.DefaultInstance})
	require.True(t, ok, "the health aggregator must be selected")
	aggregated, ok := aggregator.Primary().(*health.Plugin)
	require.True(t, ok)

	report := aggregated.Check(context.Background(), health.Readiness)
	assert.Equal(t, health.Up, report.Status)
	names := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		names = append(names, check.Name)
	}
	assert.Equal(t, []string{"gorm-health/primary", "gorm-health/readonly"}, names)
}
