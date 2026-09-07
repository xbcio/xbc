package tracing

import (
	"context"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

func TestConfigNormalizesDefaultsAndCopiesInput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Exporter.Protocol = ProtocolHTTP
	cfg.Exporter.Endpoint = ""
	cfg.ResourceAttributes = map[string]string{"team": "payments"}
	cfg.Exporter.Headers = map[string]string{"authorization": "secret"}

	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.exporter.endpoint != defaultHTTPEndpoint || normalized.exporter.protocol != ProtocolHTTP {
		t.Fatalf("HTTP exporter = %#v", normalized.exporter)
	}
	if normalized.responseHeaders.TraceIDHeader != "X-Trace-Id" ||
		normalized.responseHeaders.SpanIDHeader != "X-Span-Id" {
		t.Fatalf("response headers = %#v", normalized.responseHeaders)
	}
	cfg.ResourceAttributes["team"] = "changed"
	cfg.Exporter.Headers["authorization"] = "changed"
	if normalized.resourceAttributes["team"] != "payments" || normalized.exporter.headers["Authorization"] != "secret" {
		t.Fatal("normalized config aliases caller-owned maps")
	}
}

func TestConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"sample ratio below zero", func(c *Config) { c.SampleRatio = -0.01 }},
		{"sample ratio above one", func(c *Config) { c.SampleRatio = 1.01 }},
		{"sample ratio NaN", func(c *Config) { c.SampleRatio = math.NaN() }},
		{"unknown protocol", func(c *Config) { c.Exporter.Protocol = "zipkin" }},
		{"endpoint without port", func(c *Config) { c.Exporter.Endpoint = "collector" }},
		{"endpoint credentials", func(c *Config) { c.Exporter.Endpoint = "https://user:pass@collector:4318" }},
		{"secure HTTP URL", func(c *Config) { c.Exporter.Endpoint = "http://collector:4318" }},
		{"insecure HTTPS URL", func(c *Config) { c.Exporter.Insecure = true; c.Exporter.Endpoint = "https://collector:4318" }},
		{"header newline", func(c *Config) { c.Exporter.Headers = map[string]string{"Authorization": "a\nb"} }},
		{"duplicate normalized header", func(c *Config) { c.Exporter.Headers = map[string]string{"x-token": "a", "X-Token": "b"} }},
		{"client certificate without key", func(c *Config) { c.Exporter.TLS.CertFile = "client.pem" }},
		{"TLS with insecure", func(c *Config) { c.Exporter.Insecure = true; c.Exporter.TLS.ServerName = "collector" }},
		{"zero exporter timeout", func(c *Config) { c.Exporter.Timeout = 0 }},
		{"zero queue", func(c *Config) { c.Batch.MaxQueueSize = 0 }},
		{"batch exceeds queue", func(c *Config) { c.Batch.MaxExportBatchSize = c.Batch.MaxQueueSize + 1 }},
		{"zero batch timeout", func(c *Config) { c.Batch.BatchTimeout = 0 }},
		{"unknown propagation", func(c *Config) { c.Propagation = []string{"b3"} }},
		{"duplicate propagation", func(c *Config) { c.Propagation = []string{"baggage", "BAGGAGE"} }},
		{"same response header", func(c *Config) { c.ResponseHeaders.TraceIDHeader = "X-ID"; c.ResponseHeaders.SpanIDHeader = "x-id" }},
		{"blank resource key", func(c *Config) { c.ResourceAttributes = map[string]string{" ": "value"} }},
		{"duplicate normalized resource key", func(c *Config) { c.ResourceAttributes = map[string]string{"team": "a", " team ": "b"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate(%#v) succeeded", cfg)
			}
		})
	}
}

func TestResourceContainsConfiguredServiceAttributes(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) {
		c.ServiceName = "checkout"
		c.ServiceVersion = "2.4.1"
		c.Environment = "staging"
		c.ResourceAttributes = map[string]string{"service.namespace": "commerce", "team": "payments"}
		c.Batch.BatchTimeout = time.Hour
	}, exporter)

	_, span := p.Handle().Tracer("resource-test").Start(context.Background(), "resource")
	span.End()
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d", len(spans))
	}
	set := spans[0].Resource.Set()
	assertResourceString(t, set, semconv.ServiceNameKey, "checkout")
	assertResourceString(t, set, semconv.ServiceVersionKey, "2.4.1")
	assertResourceString(t, set, semconv.DeploymentEnvironmentNameKey, "staging")
	assertResourceString(t, set, attribute.Key("service.namespace"), "commerce")
	assertResourceString(t, set, attribute.Key("team"), "payments")
}

func TestInitTLSFileFailureDoesNotPublishHandle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Exporter.TLS.CAFile = t.TempDir() + "/missing-ca.pem"
	p, err := New(cfg)
	if err == nil {
		t.Fatal("New succeeded with missing CA file")
	}
	if p != nil {
		t.Fatal("failed New published tracing state")
	}
}

func assertResourceString(t *testing.T, set *attribute.Set, key attribute.Key, want string) {
	t.Helper()
	got, ok := set.Value(key)
	if !ok || got.AsString() != want {
		t.Fatalf("resource %s = %v, present=%v, want %q", key, got, ok, want)
	}
}
