package metrics

import "testing"

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
	if !normalized.endpoint.enabled {
		t.Fatal("normalized endpoint did not keep the enabled flag")
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
