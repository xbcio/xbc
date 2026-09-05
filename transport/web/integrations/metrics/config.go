package metrics

import (
	"crypto/sha256"
	"fmt"
	"math"
	"net/http"
	"path"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultNamespace = "xbc"
	defaultPath      = "/metrics"
	defaultHeader    = "X-XBC-Metrics-Token"
)

// Config is bound from plugins.metrics. The scrape endpoint is deliberately
// disabled by default; enabling it still requires a direct loopback peer or a
// secret token of at least 32 bytes.
type Config struct {
	Namespace       string     `yaml:"namespace" default:"xbc"`
	Subsystem       string     `yaml:"subsystem"`
	DurationBuckets []float64  `yaml:"duration_buckets"`
	HTTP            HTTPConfig `yaml:"http"`
}

// HTTPConfig controls the optional Prometheus exposition endpoint. Forwarded
// headers are never trusted for AllowLoopback decisions.
type HTTPConfig struct {
	Enabled       bool   `yaml:"enabled" default:"false"`
	Path          string `yaml:"path" default:"/metrics"`
	Header        string `yaml:"header" default:"X-XBC-Metrics-Token"`
	Token         string `yaml:"token" mask:"true"`
	AllowLoopback bool   `yaml:"allow_loopback" default:"true"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		Namespace:       defaultNamespace,
		DurationBuckets: append([]float64(nil), prometheus.DefBuckets...),
		HTTP: HTTPConfig{
			Path:          defaultPath,
			Header:        defaultHeader,
			AllowLoopback: true,
		},
	}
}

// Validate checks metric naming, histogram bounds, and endpoint security.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type endpointConfig struct {
	enabled       bool
	path          string
	header        string
	tokenDigest   [sha256.Size]byte
	hasToken      bool
	allowLoopback bool
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

	header := strings.TrimSpace(cfg.HTTP.Header)
	if header == "" {
		header = defaultHeader
	}
	if !validHeaderName(header) {
		return normalizedConfig{}, fmt.Errorf("metrics: http.header is not a valid HTTP header name")
	}
	if cfg.HTTP.Token != "" && len([]byte(cfg.HTTP.Token)) < 32 {
		return normalizedConfig{}, fmt.Errorf("metrics: http.token must contain at least 32 bytes")
	}
	if cfg.HTTP.Enabled && cfg.HTTP.Token == "" && !cfg.HTTP.AllowLoopback {
		return normalizedConfig{}, fmt.Errorf("metrics: enabled HTTP endpoint requires a token or allow_loopback")
	}

	endpoint := endpointConfig{
		enabled:       cfg.HTTP.Enabled,
		path:          endpointPath,
		header:        http.CanonicalHeaderKey(header),
		hasToken:      cfg.HTTP.Token != "",
		allowLoopback: cfg.HTTP.AllowLoopback,
	}
	if endpoint.hasToken {
		endpoint.tokenDigest = sha256.Sum256([]byte(cfg.HTTP.Token))
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

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
			continue
		default:
			return false
		}
	}
	return true
}
