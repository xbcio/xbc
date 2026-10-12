package placement

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// statsCollector publishes this placement's Stats snapshot as the stable
// series the placement recipe documents:
//
//	xbc_workload_held{workload}
//	xbc_workload_lease_age_seconds{workload}
//	xbc_workload_lease_renew_failures_total
//	xbc_workload_declared_replicas{workload,exclusive}
//	xbc_placement_standby
//
// The names are this module's API rather than an application's: dashboards and
// alert rules are written against them, so they are spelled out here and are
// deliberately not derived from the metrics plugin's namespace and subsystem
// settings, which belong to application metrics.
type statsCollector struct {
	stats func() Stats

	held          *prometheus.Desc
	leaseAge      *prometheus.Desc
	renewFailures *prometheus.Desc
	declared      *prometheus.Desc
	standby       *prometheus.Desc
}

func newStatsCollector(stats func() Stats) *statsCollector {
	return &statsCollector{
		stats: stats,
		held: prometheus.NewDesc("xbc_workload_held",
			"One series per workload this process holds a lease-backed slot for.",
			[]string{"workload"}, nil),
		leaseAge: prometheus.NewDesc("xbc_workload_lease_age_seconds",
			"Age in seconds of the lease this process holds for a workload.",
			[]string{"workload"}, nil),
		renewFailures: prometheus.NewDesc("xbc_workload_lease_renew_failures_total",
			"Lease renewals this process has not had confirmed, cumulative.",
			nil, nil),
		declared: prometheus.NewDesc("xbc_workload_declared_replicas",
			"Replicas and exclusivity this process's placement declaration admits per workload.",
			[]string{"workload", "exclusive"}, nil),
		standby: prometheus.NewDesc("xbc_placement_standby",
			"1 when this process won no slot and hosts only workload-free plugins, 0 otherwise.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector. Every descriptor is sent because
// every series belongs to the collector's identity: a snapshot with no held
// slots still exports the renewal-failure counter and the standby gauge.
func (c *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.held
	ch <- c.leaseAge
	ch <- c.renewFailures
	ch <- c.declared
	ch <- c.standby
}

// Collect implements prometheus.Collector over one Stats snapshot.
//
// The snapshot is read once per scrape, so the numbers within one response
// consistently describe the same held set, and no state is kept between
// scrapes: a slot that was given back simply stops being reported.
func (c *statsCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.stats()
	for _, held := range stats.Held {
		workload := held.Workload.String()
		ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, 1, workload)
		ch <- prometheus.MustNewConstMetric(c.leaseAge, prometheus.GaugeValue, held.Age.Seconds(), workload)
	}
	ch <- prometheus.MustNewConstMetric(c.renewFailures, prometheus.CounterValue, float64(stats.RenewFailures))
	for _, declared := range stats.Declared {
		ch <- prometheus.MustNewConstMetric(c.declared, prometheus.GaugeValue, float64(declared.Replicas),
			declared.Workload.String(), strconv.FormatBool(declared.Exclusive))
	}
	standby := 0.0
	if stats.Standby {
		standby = 1
	}
	ch <- prometheus.MustNewConstMetric(c.standby, prometheus.GaugeValue, standby)
}
