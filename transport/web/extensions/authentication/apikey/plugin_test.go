package apikey

import (
	"reflect"
	"testing"

	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web"
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

// TestBundleSelectsTheNeutralAuthenticator pins the production spelling: a
// composition root that selects apikey.Bundle() from this package gets the
// authenticator too, so it never has to know the plugin was split.
func TestBundleSelectsTheNeutralAuthenticator(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(apikeycore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral authenticator then the Web extractor", got)
	}
}

// TestDefinitionExportsExactlyTheCredentialExtractor keeps the authenticator
// contract on the neutral half. Exporting it here as well would register the
// apikey scheme twice, which the Web authentication middleware refuses.
func TestDefinitionExportsExactlyTheCredentialExtractor(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := reflect.TypeOf((*web.CredentialExtractor)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != want {
		t.Fatalf("Definition contracts = %+v, want exactly web.CredentialExtractor", descriptor.Contracts)
	}
}
