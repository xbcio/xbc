package tracing

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"

	defaultServiceName   = "xbc"
	defaultGRPCEndpoint  = "localhost:4317"
	defaultHTTPEndpoint  = "localhost:4318"
	defaultTraceIDHeader = "X-Trace-ID"
	defaultSpanIDHeader  = "X-Span-ID"
)

// Config is bound from plugins.tracing.
type Config struct {
	ServiceName        string            `yaml:"service_name" default:"xbc"`
	ServiceVersion     string            `yaml:"service_version"`
	Environment        string            `yaml:"environment"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
	SampleRatio        float64           `yaml:"sample_ratio" default:"1"`
	Exporter           ExporterConfig    `yaml:"exporter"`
	Batch              BatchConfig       `yaml:"batch"`
	Propagation        []string          `yaml:"propagation"`
	ResponseHeaders    ResponseHeaders   `yaml:"response_headers"`
}

// ExporterConfig controls the OTLP transport. Endpoint is always passed
// explicitly, so ambient OTEL exporter environment variables cannot redirect
// this plugin's private pipeline.
type ExporterConfig struct {
	Protocol string            `yaml:"protocol" default:"grpc"`
	Endpoint string            `yaml:"endpoint" default:"localhost:4317"`
	Headers  map[string]string `yaml:"headers" mask:"true"`
	Insecure bool              `yaml:"insecure" default:"false"`
	Timeout  time.Duration     `yaml:"timeout" default:"10s"`
	TLS      TLSConfig         `yaml:"tls"`
}

// TLSConfig configures TLS 1.2+ and optional private CA or mutual TLS files.
// TLS settings are rejected when Exporter.Insecure is true.
type TLSConfig struct {
	ServerName string `yaml:"server_name"`
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
}

// BatchConfig controls the SDK batch span processor.
type BatchConfig struct {
	MaxQueueSize       int           `yaml:"max_queue_size" default:"2048"`
	MaxExportBatchSize int           `yaml:"max_export_batch_size" default:"512"`
	BatchTimeout       time.Duration `yaml:"batch_timeout" default:"5s"`
	ExportTimeout      time.Duration `yaml:"export_timeout" default:"30s"`
	BlockOnQueueFull   bool          `yaml:"block_on_queue_full" default:"false"`
}

// ResponseHeaders controls trace correlation headers written before the
// application handler runs.
type ResponseHeaders struct {
	Enabled       bool   `yaml:"enabled" default:"true"`
	TraceIDHeader string `yaml:"trace_id_header" default:"X-Trace-ID"`
	SpanIDHeader  string `yaml:"span_id_header" default:"X-Span-ID"`
}

// DefaultConfig returns the same safe defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		ServiceName: defaultServiceName,
		SampleRatio: 1,
		Exporter: ExporterConfig{
			Protocol: ProtocolGRPC,
			Endpoint: defaultGRPCEndpoint,
			Timeout:  10 * time.Second,
		},
		Batch: BatchConfig{
			MaxQueueSize:       2048,
			MaxExportBatchSize: 512,
			BatchTimeout:       5 * time.Second,
			ExportTimeout:      30 * time.Second,
		},
		Propagation: []string{"tracecontext", "baggage"},
		ResponseHeaders: ResponseHeaders{
			Enabled:       true,
			TraceIDHeader: defaultTraceIDHeader,
			SpanIDHeader:  defaultSpanIDHeader,
		},
	}
}

// Validate checks all configuration that does not require reading TLS files.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// building an exporter and returns it unchanged so the Definition factory can
// rely on already-defaulted, already-validated values.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

type normalizedConfig struct {
	serviceName        string
	serviceVersion     string
	environment        string
	resourceAttributes map[string]string
	sampleRatio        float64
	exporter           normalizedExporterConfig
	batch              BatchConfig
	propagation        []string
	responseHeaders    ResponseHeaders
}

type normalizedExporterConfig struct {
	protocol    string
	endpoint    string
	endpointURL bool
	headers     map[string]string
	insecure    bool
	timeout     time.Duration
	tls         TLSConfig
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	if len(serviceName) > 256 {
		return normalizedConfig{}, fmt.Errorf("tracing: service_name exceeds 256 bytes")
	}
	serviceVersion := strings.TrimSpace(cfg.ServiceVersion)
	environment := strings.TrimSpace(cfg.Environment)
	if len(serviceVersion) > 256 || len(environment) > 256 {
		return normalizedConfig{}, fmt.Errorf("tracing: service_version and environment must not exceed 256 bytes")
	}
	if math.IsNaN(cfg.SampleRatio) || math.IsInf(cfg.SampleRatio, 0) || cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return normalizedConfig{}, fmt.Errorf("tracing: sample_ratio must be between 0 and 1")
	}

	attrs, err := normalizeResourceAttributes(cfg.ResourceAttributes)
	if err != nil {
		return normalizedConfig{}, err
	}
	exporter, err := normalizeExporter(cfg.Exporter)
	if err != nil {
		return normalizedConfig{}, err
	}
	batch, err := normalizeBatch(cfg.Batch)
	if err != nil {
		return normalizedConfig{}, err
	}
	propagators, err := normalizePropagation(cfg.Propagation)
	if err != nil {
		return normalizedConfig{}, err
	}
	responseHeaders, err := normalizeResponseHeaders(cfg.ResponseHeaders)
	if err != nil {
		return normalizedConfig{}, err
	}
	return normalizedConfig{
		serviceName: serviceName, serviceVersion: serviceVersion,
		environment: environment, resourceAttributes: attrs,
		sampleRatio: cfg.SampleRatio, exporter: exporter, batch: batch,
		propagation: propagators, responseHeaders: responseHeaders,
	}, nil
}

func normalizeResourceAttributes(input map[string]string) (map[string]string, error) {
	if len(input) > 128 {
		return nil, fmt.Errorf("tracing: resource_attributes cannot contain more than 128 entries")
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(input))
	for _, rawKey := range keys {
		key := strings.TrimSpace(rawKey)
		value := input[rawKey]
		if key == "" || !attribute.Key(key).Defined() || len(key) > 256 {
			return nil, fmt.Errorf("tracing: invalid resource attribute key %q", rawKey)
		}
		if len(value) > 4096 {
			return nil, fmt.Errorf("tracing: resource attribute %q exceeds 4096 bytes", key)
		}
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("tracing: duplicate normalized resource attribute key %q", key)
		}
		out[key] = value
	}
	return out, nil
}

func normalizeExporter(cfg ExporterConfig) (normalizedExporterConfig, error) {
	protocol := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if protocol == "" {
		protocol = ProtocolGRPC
	}
	if protocol != ProtocolGRPC && protocol != ProtocolHTTP {
		return normalizedExporterConfig{}, fmt.Errorf("tracing: exporter.protocol must be grpc or http")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultGRPCEndpoint
		if protocol == ProtocolHTTP {
			endpoint = defaultHTTPEndpoint
		}
	}
	isURL, err := validateEndpoint(endpoint, cfg.Insecure)
	if err != nil {
		return normalizedExporterConfig{}, err
	}
	if cfg.Timeout <= 0 {
		return normalizedExporterConfig{}, fmt.Errorf("tracing: exporter.timeout must be greater than zero")
	}
	tlsCfg := TLSConfig{
		ServerName: strings.TrimSpace(cfg.TLS.ServerName),
		CAFile:     strings.TrimSpace(cfg.TLS.CAFile),
		CertFile:   strings.TrimSpace(cfg.TLS.CertFile),
		KeyFile:    strings.TrimSpace(cfg.TLS.KeyFile),
	}
	if (tlsCfg.CertFile == "") != (tlsCfg.KeyFile == "") {
		return normalizedExporterConfig{}, fmt.Errorf("tracing: exporter.tls.cert_file and key_file must be configured together")
	}
	if cfg.Insecure && (tlsCfg.ServerName != "" || tlsCfg.CAFile != "" || tlsCfg.CertFile != "") {
		return normalizedExporterConfig{}, fmt.Errorf("tracing: exporter TLS settings cannot be used with insecure transport")
	}
	headers, err := normalizeHeaders(cfg.Headers)
	if err != nil {
		return normalizedExporterConfig{}, err
	}
	return normalizedExporterConfig{
		protocol: protocol, endpoint: endpoint, endpointURL: isURL,
		headers: headers, insecure: cfg.Insecure, timeout: cfg.Timeout, tls: tlsCfg,
	}, nil
}

func validateEndpoint(endpoint string, insecure bool) (bool, error) {
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
			return false, fmt.Errorf("tracing: exporter.endpoint must be a valid URL without credentials or fragment")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return false, fmt.Errorf("tracing: exporter.endpoint URL scheme must be http or https")
		}
		if insecure && u.Scheme != "http" {
			return false, fmt.Errorf("tracing: insecure exporter endpoint URL must use http")
		}
		if !insecure && u.Scheme != "https" {
			return false, fmt.Errorf("tracing: secure exporter endpoint URL must use https")
		}
		return true, nil
	}
	if strings.ContainsAny(endpoint, "/?#") {
		return false, fmt.Errorf("tracing: exporter.endpoint must be host:port or an HTTP(S) URL")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return false, fmt.Errorf("tracing: exporter.endpoint must include host and port")
	}
	return false, nil
}

func normalizeHeaders(input map[string]string) (map[string]string, error) {
	if len(input) > 64 {
		return nil, fmt.Errorf("tracing: exporter.headers cannot contain more than 64 entries")
	}
	out := make(map[string]string, len(input))
	for rawName, value := range input {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("tracing: invalid exporter header %q", rawName)
		}
		if _, duplicate := out[name]; duplicate {
			return nil, fmt.Errorf("tracing: duplicate normalized exporter header %q", name)
		}
		out[name] = value
	}
	return out, nil
}

func normalizeBatch(cfg BatchConfig) (BatchConfig, error) {
	if cfg.MaxQueueSize <= 0 || cfg.MaxQueueSize > 1_000_000 {
		return BatchConfig{}, fmt.Errorf("tracing: batch.max_queue_size must be between 1 and 1000000")
	}
	if cfg.MaxExportBatchSize <= 0 || cfg.MaxExportBatchSize > cfg.MaxQueueSize {
		return BatchConfig{}, fmt.Errorf("tracing: batch.max_export_batch_size must be between 1 and max_queue_size")
	}
	if cfg.BatchTimeout <= 0 || cfg.ExportTimeout <= 0 {
		return BatchConfig{}, fmt.Errorf("tracing: batch timeouts must be greater than zero")
	}
	return cfg, nil
}

func normalizePropagation(input []string) ([]string, error) {
	if len(input) == 0 {
		input = []string{"tracecontext", "baggage"}
	}
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, item := range input {
		name := strings.ToLower(strings.TrimSpace(item))
		if name != "tracecontext" && name != "baggage" {
			return nil, fmt.Errorf("tracing: unsupported propagator %q", item)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("tracing: duplicate propagator %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func normalizeResponseHeaders(cfg ResponseHeaders) (ResponseHeaders, error) {
	if !cfg.Enabled {
		return ResponseHeaders{}, nil
	}
	traceHeader := http.CanonicalHeaderKey(strings.TrimSpace(cfg.TraceIDHeader))
	if traceHeader == "" {
		traceHeader = defaultTraceIDHeader
	}
	spanHeader := http.CanonicalHeaderKey(strings.TrimSpace(cfg.SpanIDHeader))
	if spanHeader == "" {
		spanHeader = defaultSpanIDHeader
	}
	if !validHeaderName(traceHeader) || !validHeaderName(spanHeader) || strings.EqualFold(traceHeader, spanHeader) {
		return ResponseHeaders{}, fmt.Errorf("tracing: response trace/span headers must be distinct valid HTTP header names")
	}
	return ResponseHeaders{Enabled: true, TraceIDHeader: traceHeader, SpanIDHeader: spanHeader}, nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))) {
			return false
		}
	}
	return true
}
