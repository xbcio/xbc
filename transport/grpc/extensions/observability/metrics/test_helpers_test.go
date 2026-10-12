package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
)

// newTestPlugin builds the instrumentation over a neutral plugin with default
// configuration (namespace xbc, Prometheus default buckets).
func newTestPlugin(t *testing.T) (*Plugin, *metricscore.Plugin) {
	t.Helper()
	core, err := metricscore.New(metricscore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	value, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	return value, core
}

func gather(t *testing.T, core *metricscore.Plugin) []*dto.MetricFamily {
	t.Helper()
	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	return families
}

// seriesValue finds the counter or gauge value of one series.
func seriesValue(families []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, labels) {
				continue
			}
			if metric.GetCounter() != nil {
				return metric.GetCounter().GetValue(), true
			}
			return metric.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// histogramSample finds the sample count and sum of one histogram series.
func histogramSample(families []*dto.MetricFamily, name string, labels map[string]string) (count uint64, sum float64, ok bool) {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metric.GetHistogram() == nil || !labelsMatch(metric, labels) {
				continue
			}
			return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum(), true
		}
	}
	return 0, 0, false
}

func labelsMatch(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, label := range metric.GetLabel() {
		if labels[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}
