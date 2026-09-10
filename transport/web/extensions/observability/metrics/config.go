package metrics

import (
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultNamespace = "xbc"
	defaultPath      = "/metrics"
)

// Config is bound from plugins.metrics. The scrape endpoint is deliberately
// disabled by default.
type Config struct {
	Namespace       string     `yaml:"namespace" default:"xbc"`
	Subsystem       string     `yaml:"subsystem"`
	DurationBuckets []float64  `yaml:"duration_buckets"`
	HTTP            HTTPConfig `yaml:"http"`
}

// HTTPConfig controls the optional Prometheus exposition endpoint.
type HTTPConfig struct {
	Enabled bool   `yaml:"enabled" default:"false"`
	Path    string `yaml:"path" default:"/metrics"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		Namespace:       defaultNamespace,
		DurationBuckets: append([]float64(nil), prometheus.DefBuckets...),
		HTTP: HTTPConfig{
			Path: defaultPath,
		},
	}
}

// Validate checks metric naming, histogram bounds, and endpoint path.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type endpointConfig struct {
	enabled bool
	path    string
}

type normalizedConfig struct {
	namespace string
	subsystem string
	buckets   []float64
	endpoint  endpointConfig
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	if !validMetricComponent(namespace) {
		return normalizedConfig{}, fmt.Errorf("metrics: namespace %q is not a valid metric-name component", cfg.Namespace)
	}
	subsystem := strings.TrimSpace(cfg.Subsystem)
	if subsystem != "" && !validMetricComponent(subsystem) {
		return normalizedConfig{}, fmt.Errorf("metrics: subsystem %q is not a valid metric-name component", cfg.Subsystem)
	}

	buckets := append([]float64(nil), cfg.DurationBuckets...)
	if len(buckets) == 0 {
		buckets = append(buckets, prometheus.DefBuckets...)
	}
	for i, bucket := range buckets {
		if bucket <= 0 || math.IsNaN(bucket) || math.IsInf(bucket, 0) {
			return normalizedConfig{}, fmt.Errorf("metrics: duration_buckets[%d] must be finite and greater than zero", i)
		}
		if i > 0 && bucket <= buckets[i-1] {
			return normalizedConfig{}, fmt.Errorf("metrics: duration_buckets must be strictly increasing")
		}
	}

	endpointPath := strings.TrimSpace(cfg.HTTP.Path)
	if endpointPath == "" {
		endpointPath = defaultPath
	}
	if !strings.HasPrefix(endpointPath, "/") || strings.ContainsAny(endpointPath, ":*") {
		return normalizedConfig{}, fmt.Errorf("metrics: http.path must be a static absolute path")
	}
	endpointPath = path.Clean(endpointPath)
	if endpointPath == "/" {
		return normalizedConfig{}, fmt.Errorf("metrics: http.path cannot be the root path")
	}

	endpoint := endpointConfig{
		enabled: cfg.HTTP.Enabled,
		path:    endpointPath,
	}
	return normalizedConfig{
		namespace: namespace,
		subsystem: subsystem,
		buckets:   buckets,
		endpoint:  endpoint,
	}, nil
}

func validMetricComponent(value string) bool {
	if value == "" || !isASCIILetter(value[0]) && value[0] != '_' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !isASCIILetter(value[i]) && (value[i] < '0' || value[i] > '9') && value[i] != '_' {
			return false
		}
	}
	return true
}

func isASCIILetter(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}
