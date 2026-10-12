package prelude

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
	"github.com/xbcio/xbc/transport/grpc/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/grpc/extensions/reliability/health"
)

func TestBundleCombinesExactlyTheCanonicalGRPCBaseline(t *testing.T) {
	want := plugin.CombineBundles(grpctransport.Bundle(), health.Bundle(), accesslog.Bundle())
	got := Bundle()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("Bundle() did not preserve the canonical gRPC baseline")
	}
	if !reflect.DeepEqual(got, Bundle()) {
		t.Fatal("Bundle() returned unstable composition content")
	}
}
