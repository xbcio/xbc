package health

import (
	"reflect"
	"testing"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	if reflect.DeepEqual(Bundle(), plugin.Bundle{}) || !reflect.DeepEqual(Bundle(), Bundle()) {
		t.Fatal("Bundle() must return one non-empty canonical composition")
	}
}

// TestBundleSelectsTheNeutralAggregator pins the production spelling: a
// composition root that selects this Bundle gets the aggregator too.
func TestBundleSelectsTheNeutralAggregator(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(corehealth.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral aggregator then the gRPC adapter", got)
	}
}

// TestDefinitionExportsExactlyTheServiceRegistrar keeps the Contributor
// contract on the neutral half; exporting it here too would register the same
// probes twice.
func TestDefinitionExportsExactlyTheServiceRegistrar(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := reflect.TypeOf((*grpctransport.ServiceRegistrar)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != want {
		t.Fatalf("Definition contracts = %+v, want exactly grpc.ServiceRegistrar", descriptor.Contracts)
	}
}

func TestNewRequiresTheAggregator(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
}

func TestInitRequiresAPluginAndContext(t *testing.T) {
	var zero *Plugin
	if err := zero.Init(nil); err == nil {
		t.Fatal("zero plugin Init succeeded")
	}
	p := newPlugin(&fakeProber{})
	if err := p.Init(nil); err == nil {
		t.Fatal("Init(nil) succeeded")
	}
}
