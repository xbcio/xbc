package metrics

import (
	"reflect"
	"testing"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

// TestBundleSelectsTheNeutralMetricsPlugin pins the production spelling: a
// composition root that selects metrics.Bundle() from this package gets the
// neutral metrics plugin too, so it never has to know the registry was split
// out.
func TestBundleSelectsTheNeutralMetricsPlugin(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(metricscore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral metrics plugin then the gRPC instrumentation", got)
	}
}

// TestDefinitionExportsBothInterceptorChains keeps the one value contributing
// to both chains: a composition root that selects this Definition must not be
// able to get a unary-only or stream-only instrumentation.
func TestDefinitionExportsBothInterceptorChains(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	unary := reflect.TypeOf((*grpctransport.UnaryInterceptor)(nil)).Elem()
	stream := reflect.TypeOf((*grpctransport.StreamInterceptor)(nil)).Elem()
	if len(descriptor.Contracts) != 2 ||
		descriptor.Contracts[0].Type != unary ||
		descriptor.Contracts[1].Type != stream {
		t.Fatalf("Definition contracts = %+v, want exactly the unary then the stream interceptor", descriptor.Contracts)
	}
}

func TestNewRequiresAMetricsPluginWithARegistry(t *testing.T) {
	if p, err := New(nil); err == nil || p != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil instrumentation and error", p, err)
	}
	if p, err := New(new(metricscore.Plugin)); err == nil || p != nil {
		t.Fatalf("New(zero plugin) = %#v, %v; want nil instrumentation and error", p, err)
	}
}

// TestNewRegistersTheDocumentedSeries pins the three instrument names and
// their bounded label sets, the contract operators build dashboards and alerts
// against.
func TestNewRegistersTheDocumentedSeries(t *testing.T) {
	p, core := newTestPlugin(t)

	p.requests.WithLabelValues("svc", "Echo", "OK").Inc()
	p.duration.WithLabelValues("svc", "Echo", "OK").Observe(0.25)
	p.inflight.WithLabelValues("svc", "Echo").Inc()

	families := gather(t, core)
	for _, name := range []string{"xbc_grpc_requests_total", "xbc_grpc_request_duration_seconds", "xbc_grpc_requests_in_flight"} {
		found := false
		for _, family := range families {
			if family.GetName() == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not registered", name)
		}
	}
	if value, ok := seriesValue(families, "xbc_grpc_requests_total", map[string]string{"service": "svc", "method": "Echo", "code": "OK"}); !ok || value != 1 {
		t.Fatalf("xbc_grpc_requests_total = %v (found=%t)", value, ok)
	}
	if count, _, ok := histogramSample(families, "xbc_grpc_request_duration_seconds", map[string]string{"service": "svc", "method": "Echo", "code": "OK"}); !ok || count != 1 {
		t.Fatalf("xbc_grpc_request_duration_seconds count = %d (found=%t)", count, ok)
	}
	if value, ok := seriesValue(families, "xbc_grpc_requests_in_flight", map[string]string{"service": "svc", "method": "Echo"}); !ok || value != 1 {
		t.Fatalf("xbc_grpc_requests_in_flight = %v (found=%t)", value, ok)
	}
}

func TestOrderObservesInPhaseObserve(t *testing.T) {
	p, _ := newTestPlugin(t)
	order := p.Order()
	if order.Phase != grpctransport.PhaseObserve {
		t.Fatalf("Order().Phase = %v, want PhaseObserve", order.Phase)
	}
	if len(order.Before) != 0 || len(order.After) != 0 {
		t.Fatalf("Order() = %+v, want no constraints beyond the phase", order)
	}
	if p.UnaryInterceptor() == nil || p.StreamInterceptor() == nil {
		t.Fatal("interceptor chains = nil")
	}
}
