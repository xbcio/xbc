package httpclient

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xbcio/xbc/extensions/observability/metrics"
)

// The three series one instance publishes. The instance name travels as a
// constant label rather than as a per-instance family name: several instances
// share one process metrics registry, and a Prometheus registry accepts
// several collectors describing the same family exactly when their constant
// labels differ. Each instance therefore registers the same family names and
// every sample separates by client=<instance>.
//
// URLs, hosts, and paths are deliberately absent from every label. A backend
// URL routinely carries ids and, in the deployments this module replaces,
// credentials in its query; a label that carries them would copy secrets into
// the metrics endpoint that the whole process can scrape.
const (
	metricNamespace = "http_client"

	labelClient = "client"
	labelMethod = "method"
	labelCode   = "code"
	labelReason = "reason"

	// codeError is the code label of an attempt that failed before a status
	// line arrived: a dial error, a TLS failure, a timeout, a canceled
	// context. It is a string rather than a synthetic numeric code so a
	// dashboard cannot mistake it for a status a peer sent.
	codeError = "error"

	// Retry reasons, one per decision the retry engine can make.
	reasonStatus     = "status"
	reasonRetryAfter = "retry_after"
	reasonConnection = "connection"
)

// clientMetrics publishes one instance's request, duration, and retry series.
// A nil *clientMetrics is a valid "no metrics stack configured" value: every
// method tolerates it, so the transport never branches on whether a metrics
// plugin was selected.
type clientMetrics struct {
	requests  *prometheus.CounterVec
	durations *prometheus.HistogramVec
	retries   *prometheus.CounterVec
}

// newClientMetrics builds and atomically registers the three collectors, or
// returns nil when the composition selected no metrics plugin. A partial
// registration is never possible: metrics.Registerer.Register is
// all-or-nothing, and a failure fails construction, so validate reports the
// duplicate or invalid descriptor instead of a scrape-time surprise.
func newClientMetrics(instance string, registerer metrics.Registerer) (*clientMetrics, error) {
	if registerer == nil {
		return nil, nil
	}
	constLabels := prometheus.Labels{labelClient: instance}
	value := &clientMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   metricNamespace,
			Name:        "requests_total",
			Help:        "Outbound HTTP attempts made by this client, by method and outcome code.",
			ConstLabels: constLabels,
		}, []string{labelMethod, labelCode}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   metricNamespace,
			Name:        "request_duration_seconds",
			Help:        "Duration of one outbound HTTP attempt, response headers included.",
			Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			ConstLabels: constLabels,
		}, []string{labelMethod}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   metricNamespace,
			Name:        "retries_total",
			Help:        "Retries scheduled by this client, by the reason the previous attempt was retried.",
			ConstLabels: constLabels,
		}, []string{labelReason}),
	}
	if err := registerer.Register(value.requests, value.durations, value.retries); err != nil {
		return nil, fmt.Errorf("http-client: register metrics: %w", err)
	}
	return value, nil
}

// observe records one attempt as it completes, before its body is read, so a
// slow response body a caller streams does not delay the sample and an
// abandoned body still appears in the counters.
func (m *clientMetrics) observe(method, code string, duration time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(method, code).Inc()
	m.durations.WithLabelValues(method).Observe(duration.Seconds())
}

// observeRetry records one scheduled retry under the reason that classified
// the previous attempt.
func (m *clientMetrics) observeRetry(reason string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(reason).Inc()
}
