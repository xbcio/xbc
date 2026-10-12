package tracing

import (
	"reflect"
	"testing"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
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

// TestBundleSelectsTheNeutralTracingPlugin pins the production spelling: a
// composition root that selects tracing.Bundle() from this package gets the
// neutral tracing plugin too, so it never has to know the pipeline was split
// out.
func TestBundleSelectsTheNeutralTracingPlugin(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(tracingcore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral tracing plugin then the gRPC interceptor", got)
	}
}

// TestDefinitionExportsBothInterceptorChains keeps the one value contributing
// to both chains: a composition root that selects this Definition must not be
// able to get a unary-only or stream-only interceptor.
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

func TestNewRequiresATracingPluginWithAProvider(t *testing.T) {
	if i, err := New(nil); err == nil || i != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil interceptor and error", i, err)
	}
	if i, err := New(new(tracingcore.Plugin)); err == nil || i != nil {
		t.Fatalf("New(zero plugin) = %#v, %v; want nil interceptor and error", i, err)
	}
}

// TestNewLowerCasesTheCorrelationHeaders pins the one deliberate
// transformation of the neutral plugin's configuration: the interceptor writes
// gRPC metadata, whose keys are always lower case, from header names that are
// configured as HTTP spellings.
func TestNewLowerCasesTheCorrelationHeaders(t *testing.T) {
	cfg := tracingcore.DefaultConfig()
	cfg.Exporter.Insecure = true
	cfg.ResponseHeaders.TraceIDHeader = "X-Request-Trace"
	core, err := tracingcore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Stop(t.Context()) })

	i, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	if i.responseHeaders.TraceIDHeader != "x-request-trace" || i.responseHeaders.SpanIDHeader != "x-span-id" {
		t.Fatalf("interceptor headers = %#v, want lower-cased forms", i.responseHeaders)
	}
	if i.responseHeaders.Enabled != core.ResponseHeaders().Enabled {
		t.Fatalf("interceptor headers = %#v, want enabled=%v from the plugin", i.responseHeaders, core.ResponseHeaders().Enabled)
	}
	if i.handle == nil || i.handle.Provider() != core.Provider() {
		t.Fatal("interceptor does not record into the tracing plugin's provider")
	}
}

func TestOrderRunsAheadOfMetrics(t *testing.T) {
	i, _, _ := newTestInterceptor(t)
	order := i.Order()
	if order.Phase != grpctransport.PhaseObserve {
		t.Fatalf("Order().Phase = %v, want PhaseObserve", order.Phase)
	}
	want := []grpctransport.OrderRef{grpctransport.Prefer(metricsKey)}
	if !reflect.DeepEqual(order.Before, want) {
		t.Fatalf("Order().Before = %#v, want %#v", order.Before, want)
	}
	if i.UnaryInterceptor() == nil || i.StreamInterceptor() == nil {
		t.Fatal("interceptor chains = nil")
	}
}
