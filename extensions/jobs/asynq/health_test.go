package asynq

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestHealthChecksContributeOneReadinessCheck(t *testing.T) {
	server := miniredis.RunT(t)
	host := newTestHost()
	t.Cleanup(host.stopTasks)

	p := newTestPlugin(t, validTestConfig(server.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })

	checks := p.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if checks[0].Name != "" {
		t.Fatalf("the check must be named by the contributing instance, got %q", checks[0].Name)
	}
	if checks[0].Kind != health.Readiness {
		t.Fatalf("got kind %q, want readiness", checks[0].Kind)
	}
	if checks[0].Timeout != 0 {
		t.Fatalf("the check must inherit the plugins.health timeout, got %s", checks[0].Timeout)
	}

	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if !report.Healthy() {
		t.Fatalf("a reachable Redis must report up, got %+v", report)
	}
}

// A plugin whose Init has not run holds no connection. Reporting that as down
// keeps readiness honest instead of turning a lifecycle state into a probe
// defect, and it distinguishes the two states in the message.
func TestHealthCheckReportsDownBeforeInitAndAfterStop(t *testing.T) {
	server := miniredis.RunT(t)
	host := newTestHost()
	t.Cleanup(host.stopTasks)

	p := newTestPlugin(t, validTestConfig(server.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))

	report := health.Check(context.Background(), health.Readiness, p.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("an uninitialized plugin must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
	if message := report.Checks[0].Error.Error(); !strings.Contains(message, "not initialized") {
		t.Fatalf("got error %q, want it to name the uninitialized state", message)
	}

	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	report = health.Check(context.Background(), health.Readiness, p.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a stopped plugin must report down, got %+v", report)
	}
	if message := report.Checks[0].Error.Error(); !strings.Contains(message, "has stopped") {
		t.Fatalf("got error %q, want it to name the stopped state", message)
	}
}

func TestHealthCheckReportsDownForAnUnreachableRedis(t *testing.T) {
	server := miniredis.RunT(t)
	host := newTestHost()
	t.Cleanup(host.stopTasks)

	p := newTestPlugin(t, validTestConfig(server.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	server.Close()

	report := health.Check(context.Background(), health.Readiness, p.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("an unreachable Redis must report down, got %+v", report)
	}
}

// TestPlanExportsTheHealthContract proves the contract reaches assembly: the
// primary carries it, so a configured instance becomes a contributor without
// the composition root wiring anything.
func TestPlanExportsTheHealthContract(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"asynq": map[string]any{"redis": map[string]any{"addr": "127.0.0.1:6379"}},
		},
	}, "XBC_ASYNQ_HEALTH_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}

	contributors := plan.Contracts(reflect.TypeOf((*health.Contributor)(nil)).Elem())
	want := []plugin.Identity{{Plugin: Key, Instance: plugin.DefaultInstance}}
	if !reflect.DeepEqual(contributors, want) {
		t.Fatalf("got health contributors %v, want %v", contributors, want)
	}
}
