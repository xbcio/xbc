package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xbcio/xbc/transport/web"
)

type brokenCollector struct{}

func (brokenCollector) Describe(chan<- *prometheus.Desc) {}
func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("broken_metric", "broken", nil, nil), assertError("collection failed"))
}

type assertError string

func (e assertError) Error() string { return string(e) }

func TestEndpointExposition(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = true
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.GET("/metrics", web.Handle(p.handleMetrics))
	probe := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "endpoint_probe_total",
		Help: "Metric used to verify exposition.",
	})
	probe.Inc()
	if err := p.Registry().Register(probe); err != nil {
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
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = true
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Registry().Register(brokenCollector{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	engine := gin.New()
	engine.GET("/metrics", web.Handle(p.handleMetrics))
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDisabledEndpointReturnsNotFound(t *testing.T) {
	p, err := newPlugin(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.GET("/metrics", web.Handle(p.handleMetrics))
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

func TestNormalizeConfigPropagatesCustomEndpointPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = true
	cfg.HTTP.Path = "/ops/metrics"
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.endpoint.path != "/ops/metrics" {
		t.Fatalf("endpoint path = %q, want the configured plugins.metrics.http.path", normalized.endpoint.path)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []Config{
		{Namespace: "bad-name", HTTP: HTTPConfig{Path: defaultPath}},
		{Namespace: defaultNamespace, DurationBuckets: []float64{1, 1}, HTTP: HTTPConfig{Path: defaultPath}},
		{Namespace: defaultNamespace, HTTP: HTTPConfig{Path: "/metrics/:tenant"}},
	}
	for _, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", cfg)
		}
	}
}
