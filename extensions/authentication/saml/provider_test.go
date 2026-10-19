package saml

import "testing"

func TestACSURLReturnsTheConfiguredTrustAnchor(t *testing.T) {
	provider := newTestProvider(t, nil)
	if got, want := provider.ACSURL(), testConfig().ACSURL; got != want {
		t.Fatalf("ACSURL() = %q, want %q", got, want)
	}
}

func TestACSURLIsEmptyForANilProvider(t *testing.T) {
	var provider *Provider
	if got := provider.ACSURL(); got != "" {
		t.Fatalf("nil Provider.ACSURL() = %q, want the empty string", got)
	}
}
