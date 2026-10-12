package jwt

import (
	"reflect"
	"testing"

	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
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

// TestBundleSelectsTheNeutralAuthenticator pins the production spelling: a
// composition root that selects this Bundle gets the authenticator too.
func TestBundleSelectsTheNeutralAuthenticator(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(jwtcore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral authenticator then the gRPC extractor", got)
	}
}

// TestDefinitionExportsExactlyTheCredentialExtractor keeps the authenticator
// contract on the neutral half; exporting it here too would register the
// jwt scheme twice, which the Server refuses.
func TestDefinitionExportsExactlyTheCredentialExtractor(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := reflect.TypeOf((*grpctransport.CredentialExtractor)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != want {
		t.Fatalf("Definition contracts = %+v, want exactly grpc.CredentialExtractor", descriptor.Contracts)
	}
}

func TestNewRequiresTheAuthenticator(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
}
