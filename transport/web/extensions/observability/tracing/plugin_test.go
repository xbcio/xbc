package tracing

import (
	"reflect"
	"testing"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
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
// neutral tracing plugin too, so it never has to know the plugin was split.
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
		t.Fatalf("Bundle() definitions = %v, want the neutral tracing plugin then the Web middleware", got)
	}
}

// TestDefinitionExportsExactlyTheMiddleware keeps the Handle contract on the
// neutral half, which is where every consumer, Web or not, resolves it.
func TestDefinitionExportsExactlyTheMiddleware(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := reflect.TypeOf((*web.Middleware)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != want {
		t.Fatalf("Definition contracts = %+v, want exactly web.Middleware", descriptor.Contracts)
	}
}

func TestNewRequiresATracingPluginWithAProvider(t *testing.T) {
	if m, err := New(nil); err == nil || m != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil middleware and error", m, err)
	}
	if m, err := New(new(tracingcore.Plugin)); err == nil || m != nil {
		t.Fatalf("New(zero plugin) = %#v, %v; want nil middleware and error", m, err)
	}
}

func TestNewReadsTheCorrelationHeadersFromTheTracingPlugin(t *testing.T) {
	cfg := tracingcore.DefaultConfig()
	cfg.Exporter.Insecure = true
	cfg.ResponseHeaders.TraceIDHeader = "X-Request-Trace"
	core, err := tracingcore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Stop(t.Context()) })

	m, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	if m.responseHeaders != core.ResponseHeaders() || m.responseHeaders.TraceIDHeader != "X-Request-Trace" {
		t.Fatalf("middleware headers = %#v, want %#v", m.responseHeaders, core.ResponseHeaders())
	}
	if m.handle == nil || m.handle.Provider() != core.Provider() {
		t.Fatal("middleware does not record into the tracing plugin's provider")
	}
}

func TestOrderRunsAheadOfMetricsAndAccessLogging(t *testing.T) {
	m, _, _ := newTestMiddleware(t)
	if m.Order().Phase != web.PhaseObserve {
		t.Fatalf("Order().Phase = %v, want PhaseObserve", m.Order().Phase)
	}
	want := []web.OrderRef{web.Prefer(metricsKey), web.Prefer(accesslog.Key)}
	if !reflect.DeepEqual(m.Order().Before, want) {
		t.Fatalf("Order().Before = %#v, want %#v", m.Order().Before, want)
	}
	if m.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
}
