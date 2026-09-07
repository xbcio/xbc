package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

type brokenCollector struct{}

func (brokenCollector) Describe(chan<- *prometheus.Desc) {}
func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("broken_metric", "broken", nil, nil), assertError("collection failed"))
}

type assertError string

func (e assertError) Error() string { return string(e) }

func TestEndpointSecurityAndExposition(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = true
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.GET("/metrics", p.handleMetrics)
	probe := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "endpoint_probe_total",
		Help: "Metric used to verify exposition.",
	})
	probe.Inc()
	if err := p.Registry().Register(probe); err != nil {
		t.Fatal(err)
	}

	remote := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	remote.RemoteAddr = "203.0.113.8:1234"
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	denied := httptest.NewRecorder()
	engine.ServeHTTP(denied, remote)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("forwarded loopback status = %d", denied.Code)
	}

	loopback := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	loopback.RemoteAddr = "127.0.0.1:1234"
	allowed := httptest.NewRecorder()
	engine.ServeHTTP(allowed, loopback)
	if allowed.Code != http.StatusOK {
		t.Fatalf("loopback status = %d body=%s", allowed.Code, allowed.Body.String())
	}
	if !strings.Contains(allowed.Body.String(), "# HELP endpoint_probe_total") {
		t.Fatalf("missing Prometheus exposition:\n%s", allowed.Body.String())
	}
	if allowed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", allowed.Header().Get("Cache-Control"))
	}
}

func TestTokenProtectsNonLoopbackEndpoint(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = true
	cfg.HTTP.AllowLoopback = false
	cfg.HTTP.Token = token
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.GET("/metrics", p.handleMetrics)

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.RemoteAddr = "203.0.113.8:1234"
	request.Header.Set(defaultHeader, token)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("token status = %d body=%s", response.Code, response.Body.String())
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
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	engine := gin.New()
	engine.GET("/metrics", p.handleMetrics)
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []Config{
		{Namespace: "bad-name", HTTP: HTTPConfig{Path: defaultPath, Header: defaultHeader, AllowLoopback: true}},
		{Namespace: defaultNamespace, DurationBuckets: []float64{1, 1}, HTTP: HTTPConfig{Path: defaultPath, Header: defaultHeader, AllowLoopback: true}},
		{Namespace: defaultNamespace, HTTP: HTTPConfig{Enabled: true, Path: defaultPath, Header: defaultHeader}},
		{Namespace: defaultNamespace, HTTP: HTTPConfig{Enabled: true, Path: defaultPath, Header: defaultHeader, Token: "short"}},
		{Namespace: defaultNamespace, HTTP: HTTPConfig{Path: "/metrics/:tenant", Header: defaultHeader, AllowLoopback: true}},
	}
	for _, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", cfg)
		}
	}
}
