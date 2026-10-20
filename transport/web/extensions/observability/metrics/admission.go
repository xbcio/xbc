package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/transport/web"
)

// admissionCollector reports the process-level admission gate as three
// unlabelled series. The gate refuses work before any contributed middleware
// runs, so the request instruments never observe a refusal; these series are
// the only evidence that a saturated process is refusing traffic rather than
// sitting idle.
//
// It is a collector rather than three standalone instruments because
// InFlightStats is a snapshot: the ceiling, the occupancy, and the rejection
// count are only meaningful together, and a rejection count without its
// ceiling cannot distinguish a working gate from a ceiling set too low.
type admissionCollector struct {
	source     web.AdmissionStatsSource
	limit      *prometheus.Desc
	inFlight   *prometheus.Desc
	rejections *prometheus.Desc
}

var _ prometheus.Collector = (*admissionCollector)(nil)

// newAdmissionCollector builds the collector over core's configured namespace
// and subsystem, so the admission names carry the same prefix as the request
// instruments. The source must be non-nil: New rejects a nil source before
// constructing the collector.
func newAdmissionCollector(core *metricscore.Plugin, source web.AdmissionStatsSource) *admissionCollector {
	return &admissionCollector{
		source: source,
		limit: prometheus.NewDesc(
			prometheus.BuildFQName(core.Namespace(), core.Subsystem(), "http_admission_limit"),
			"Effective process-level ceiling on concurrently admitted HTTP requests.",
			nil,
			nil,
		),
		inFlight: prometheus.NewDesc(
			prometheus.BuildFQName(core.Namespace(), core.Subsystem(), "http_admission_in_flight"),
			"HTTP requests currently holding an admission slot.",
			nil,
			nil,
		),
		rejections: prometheus.NewDesc(
			prometheus.BuildFQName(core.Namespace(), core.Subsystem(), "http_admission_rejections_total"),
			"HTTP requests refused by the admission gate for want of an in-flight slot.",
			nil,
			nil,
		),
	}
}

// Describe implements prometheus.Collector. The descriptions are constant, so
// they are built once when the collector is constructed.
func (c *admissionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.limit
	ch <- c.inFlight
	ch <- c.rejections
}

// Collect implements prometheus.Collector. It reads exactly one snapshot and
// emits all three readings from it: three independent reads would let one
// scrape publish an occupancy from one instant beside a rejection count from
// another, and the values are meant to be compared with each other.
func (c *admissionCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.source.InFlightStats()
	ch <- prometheus.MustNewConstMetric(c.limit, prometheus.GaugeValue, float64(stats.Limit))
	ch <- prometheus.MustNewConstMetric(c.inFlight, prometheus.GaugeValue, float64(stats.InFlight))
	ch <- prometheus.MustNewConstMetric(c.rejections, prometheus.CounterValue, float64(stats.Rejections))
}
