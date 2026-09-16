package elasticsearch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestHealthChecksContributeOneReadinessCheck(t *testing.T) {
	client := newClientWithBackend(&stubBackend{}, time.Second)
	client.healthProbe = true

	checks := client.HealthChecks()
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
		t.Fatalf("a reachable cluster must report up, got %+v", report)
	}
}

// health_probe: false is the operator's statement that this cluster must not
// decide whether the process serves traffic. Init already honours it, so the
// readiness contribution has to honour it too.
func TestHealthChecksAreSuppressedWhenTheProbeIsDisabled(t *testing.T) {
	client := newClientWithBackend(&stubBackend{}, time.Second)
	client.healthProbe = false

	if checks := client.HealthChecks(); len(checks) != 0 {
		t.Fatalf("a disabled probe must contribute nothing, got %+v", checks)
	}
}

func TestHealthCheckReportsDownForAClosedClient(t *testing.T) {
	client := newClientWithBackend(&stubBackend{}, time.Second)
	client.healthProbe = true
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("close client: %v", err)
	}

	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a closed client must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
}

func TestHealthCheckReportsDownForAnUnavailableCluster(t *testing.T) {
	client := newClientWithBackend(&stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusServiceUnavailable, `{}`), nil
	}}, time.Second)
	client.healthProbe = true

	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a cluster answering 503 must report down, got %+v", report)
	}
}

// TestAssembledHealthPluginNamesTheContributingInstance proves the contract
// export reaches the aggregator and that a named instance is distinguishable:
// the primary value carries the contract, so the aggregator names each check
// after the instance identity that produced it.
func TestAssembledHealthPluginNamesTheContributingInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Elastic-Product", "Elasticsearch")
		_, _ = io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)

	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"elasticsearch": map[string]any{
				"search": map[string]any{"addresses": []any{server.URL}},
			},
		},
	}, "XBC_ELASTICSEARCH_HEALTH_TEST_UNSET_")
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
			return plugin.NewRuntimeContext(newFakeHost(), identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "search"}); ok {
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
		t.Fatalf("a reachable configured cluster must report up, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Name != "elasticsearch[search]" {
		t.Fatalf("got checks %+v, want one named elasticsearch[search]", report.Checks)
	}
}
