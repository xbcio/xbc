package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

type brokenCollector struct{}

func (brokenCollector) Describe(chan<- *prometheus.Desc) {}
func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("broken_metric", "broken", nil, nil), assertError("collection failed"))
}

type assertError string

func (e assertError) Error() string { return string(e) }

func newEnabledPluginForTest(t *testing.T) (*Plugin, *metricscore.Plugin) {
	t.Helper()
	cfg := metricscore.DefaultConfig()
	cfg.HTTP.Enabled = true
	core, err := metricscore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	value, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	return value, core
}

func TestEndpointExposition(t *testing.T) {
	p, core := newEnabledPluginForTest(t)

	engine := enginetest.New()
	engine.GET("/metrics", p.handleMetrics)
	probe := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "endpoint_probe_total",
		Help: "Metric used to verify exposition.",
	})
	probe.Inc()
	if err := core.Register(probe); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "# HELP endpoint_probe_total") {
		t.Fatalf("missing Prometheus exposition:\n%s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestGatherFailureReturnsServerError(t *testing.T) {
	p, core := newEnabledPluginForTest(t)
	if err := core.Register(brokenCollector{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	engine := enginetest.New()
	engine.GET("/metrics", p.handleMetrics)
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDisabledEndpointReturnsNotFound(t *testing.T) {
	core := newCoreForTest(t)
	p, err := New(core)
	if err != nil {
		t.Fatal(err)
	}

	engine := enginetest.New()
	engine.GET("/metrics", p.handleMetrics)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint handler status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Fatalf("content type = %q", got)
	}
}
