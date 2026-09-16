package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestHealthDefinitionIsDistinctFromTheClientDefinition(t *testing.T) {
	var zero plugin.Definition
	if healthDefinition == zero {
		t.Fatal("healthDefinition is a zero handle")
	}
	if healthDefinition == definition {
		t.Fatal("the readiness probe must be a distinct Definition from the client")
	}
	if HealthKey == Key {
		t.Fatalf("the probe Key %q must differ from the client Key %q", HealthKey, Key)
	}
}

func TestHealthChecksReportOneReadinessCheckPerInstance(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	probe, err := newHealthProbe([]plugin.Entry[*goredis.Client]{
		{Identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, Value: client},
		{Identity: plugin.Identity{Plugin: Key, Instance: "cache"}, Value: client},
	})
	if err != nil {
		t.Fatalf("newHealthProbe: %v", err)
	}

	checks := probe.HealthChecks()
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(checks))
	}
	if checks[0].Name != "" {
		t.Fatalf("the default instance must contribute an unqualified name, got %q", checks[0].Name)
	}
	if checks[1].Name != "cache" {
		t.Fatalf("got name %q, want %q", checks[1].Name, "cache")
	}
	for _, check := range checks {
		if check.Kind != health.Readiness {
			t.Fatalf("check %q has kind %q, want readiness", check.Name, check.Kind)
		}
		if check.Timeout != 0 {
			t.Fatalf("check %q must inherit the plugins.health timeout, got %s", check.Name, check.Timeout)
		}
	}

	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if !report.Healthy() {
		t.Fatalf("a reachable server must report up, got %+v", report)
	}
}

func TestHealthChecksReportDownForAnUnreachableInstance(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	probe, err := newHealthProbe([]plugin.Entry[*goredis.Client]{
		{Identity: plugin.Identity{Plugin: Key, Instance: "cache"}, Value: client},
	})
	if err != nil {
		t.Fatalf("newHealthProbe: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	report := health.Check(context.Background(), health.Readiness, probe.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a closed client must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
}

func TestHealthProbeRejectsANilClient(t *testing.T) {
	if _, err := newHealthProbe([]plugin.Entry[*goredis.Client]{
		{Identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}},
	}); err == nil {
		t.Fatal("a nil client must fail construction instead of panicking during a probe")
	}
}

// TestAssembledHealthPluginReportsEveryConfiguredInstance is the load-bearing
// test for this Definition's existence. The probe exists because *redis.Client,
// a third-party type, cannot implement health.Contributor: assembly rejects a
// contract its declaring Definition's primary type is not assignable to. This
// test proves the second-Definition shape actually reaches the aggregator.
func TestAssembledHealthPluginReportsEveryConfiguredInstance(t *testing.T) {
	server := miniredis.RunT(t)
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis": map[string]any{
				"cache": map[string]any{"addr": server.Addr()},
			},
		},
	}, "XBC_REDIS_HEALTH_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), health.Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(healthTestHost{}, identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "cache"}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
	})

	aggregator, ok := constructed.Instance(plugin.Identity{Plugin: health.Key, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the health aggregator must be selected")
	}
	aggregated, ok := aggregator.Primary().(*health.Plugin)
	if !ok {
		t.Fatalf("health primary is %T, want *health.Plugin", aggregator.Primary())
	}

	report := aggregated.Check(context.Background(), health.Readiness)
	if report.Status != health.Up {
		t.Fatalf("a reachable configured instance must report up, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Name != "redis-health/cache" {
		t.Fatalf("got checks %+v, want one named redis-health/cache", report.Checks)
	}
}

type healthTestHost struct{}

func (healthTestHost) ExecutionContext() context.Context { return context.Background() }
func (healthTestHost) Logger() log.Logger                { return nil }
func (healthTestHost) TrafficGate() <-chan struct{}      { return nil }
func (healthTestHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}
func (healthTestHost) RequestShutdown(plugin.Identity, string) bool { return false }
