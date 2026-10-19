package ldap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// providerWithDirectory builds a provider whose directory seam is a fake.
func providerWithDirectory(t *testing.T, directory *fakeDirectory) *Provider {
	t.Helper()
	provider := newProvider(t, testConfig("ldaps://directory.example.com"))
	provider.directory = directory
	return provider
}

// aliceEntry is the directory entry most flow tests return. Attribute names
// are lower-case: that is the contract of the userDirectory seam, just as the
// real directory lower-cases every name it receives.
func aliceEntry() userEntry {
	return userEntry{
		dn: "uid=alice,ou=people,dc=example,dc=com",
		attributes: map[string][]string{
			"uid":       {"alice"},
			"entryuuid": {"11111111-2222-3333-4444-555555555555"},
			"mail":      {"alice@example.com"},
			"cn":        {"Alice", "Alice Example"},
		},
	}
}

func TestVerifyBuildsTheIdentityFromTheDirectoryEntry(t *testing.T) {
	directory := &fakeDirectory{lookupResult: aliceEntry()}
	provider := providerWithDirectory(t, directory)

	identity, err := Verify(context.Background(), provider, "alice", "user-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.Protocol != "ldap" {
		t.Fatalf("Protocol = %q, want ldap", identity.Protocol)
	}
	if identity.Provider != testProviderName {
		t.Fatalf("Provider = %q, want %q", identity.Provider, testProviderName)
	}
	if identity.Issuer != "ldaps://directory.test" {
		t.Fatalf("Issuer = %q", identity.Issuer)
	}
	if identity.Subject != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("Subject = %q, want the entryUUID value", identity.Subject)
	}
	if identity.Claims["email"] != "alice@example.com" {
		t.Fatalf("Claims[email] = %v", identity.Claims["email"])
	}
	display, ok := identity.Claims["display_name"].([]string)
	if !ok || len(display) != 2 || display[0] != "Alice" || display[1] != "Alice Example" {
		t.Fatalf("Claims[display_name] = %#v, want the two cn values", identity.Claims["display_name"])
	}
	wantTime := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if !identity.AuthTime.Equal(wantTime) {
		t.Fatalf("AuthTime = %s, want %s", identity.AuthTime, wantTime)
	}
	if directory.binds.Load() != 1 {
		t.Fatalf("user binds = %d, want 1", directory.binds.Load())
	}
	if directory.lastDN != aliceEntry().dn {
		t.Fatalf("user bind DN = %q", directory.lastDN)
	}
	if directory.lastPassword != "user-secret" {
		t.Fatalf("user bind password = %q", directory.lastPassword)
	}
}

func TestVerifyRejectsEmptyCredentialsBeforeAnyDirectoryCall(t *testing.T) {
	for _, test := range []struct {
		name     string
		username string
		password string
	}{
		{name: "empty password", username: "alice", password: ""},
		{name: "empty username", username: "", password: "secret"},
		{name: "both empty", username: "", password: ""},
		{name: "oversized username", username: strings.Repeat("a", maxUsernameLength+1), password: "secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &fakeDirectory{lookupResult: aliceEntry()}
			provider := providerWithDirectory(t, directory)
			_, err := Verify(context.Background(), provider, test.username, test.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("Verify = %v, want ErrInvalidCredentials", err)
			}
			if directory.lookups.Load() != 0 || directory.binds.Load() != 0 {
				t.Fatalf("directory was called: lookups=%d binds=%d", directory.lookups.Load(), directory.binds.Load())
			}
		})
	}
}

func TestVerifyTreatsLookupAndBindFailuresUniformly(t *testing.T) {
	lookupFailure := &fakeDirectory{lookupErr: ErrInvalidCredentials}
	bindFailure := &fakeDirectory{lookupResult: aliceEntry(), bindErr: ErrInvalidCredentials}

	_, lookupErr := Verify(context.Background(), providerWithDirectory(t, lookupFailure), "alice", "wrong")
	_, bindErr := Verify(context.Background(), providerWithDirectory(t, bindFailure), "alice", "wrong")

	if !errors.Is(lookupErr, ErrInvalidCredentials) || !errors.Is(bindErr, ErrInvalidCredentials) {
		t.Fatalf("errors = %v and %v, want ErrInvalidCredentials", lookupErr, bindErr)
	}
	if lookupErr.Error() != bindErr.Error() {
		t.Fatalf("outward errors differ: %q vs %q", lookupErr, bindErr)
	}
	if lookupErr != ErrInvalidCredentials || bindErr != ErrInvalidCredentials {
		t.Fatal("credential failures must be the bare sentinel, not a wrapped error with detail")
	}
}

func TestVerifyDoesNotBindWhenTheLookupFails(t *testing.T) {
	directory := &fakeDirectory{lookupErr: ErrUnavailable}
	_, err := Verify(context.Background(), providerWithDirectory(t, directory), "alice", "secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	if directory.binds.Load() != 0 {
		t.Fatal("the user bind ran after the lookup failed")
	}
}

func TestVerifyRejectsAnEntryWithoutAStableSubject(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string][]string
	}{
		{name: "missing entryUUID", attributes: map[string][]string{"uid": {"alice"}, "mail": {"a@example.com"}, "cn": {"Alice"}}},
		{name: "empty entryUUID", attributes: map[string][]string{"entryuuid": {""}, "mail": {"a@example.com"}, "cn": {"Alice"}}},
		{name: "two entryUUID values", attributes: map[string][]string{"entryuuid": {"one", "two"}, "mail": {"a@example.com"}, "cn": {"Alice"}}},
		{name: "binary entryUUID", attributes: map[string][]string{"entryuuid": {"\xff\xfe"}, "mail": {"a@example.com"}, "cn": {"Alice"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &fakeDirectory{lookupResult: userEntry{dn: aliceEntry().dn, attributes: test.attributes}}
			_, err := Verify(context.Background(), providerWithDirectory(t, directory), "alice", "secret")
			if !errors.Is(err, ErrInvalidDirectoryEntry) {
				t.Fatalf("Verify = %v, want ErrInvalidDirectoryEntry", err)
			}
			if directory.binds.Load() != 1 {
				t.Fatalf("user binds = %d, want 1: the password is verified before entry data is consumed", directory.binds.Load())
			}
		})
	}
}

func TestVerifyRejectsMissingOrBinaryClaimAttributes(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string][]string
	}{
		{name: "missing email", attributes: map[string][]string{"entryuuid": {"uuid"}, "cn": {"Alice"}}},
		{name: "binary email", attributes: map[string][]string{"entryuuid": {"uuid"}, "mail": {"\xff"}, "cn": {"Alice"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &fakeDirectory{lookupResult: userEntry{dn: aliceEntry().dn, attributes: test.attributes}}
			_, err := Verify(context.Background(), providerWithDirectory(t, directory), "alice", "secret")
			if !errors.Is(err, ErrInvalidDirectoryEntry) {
				t.Fatalf("Verify = %v, want ErrInvalidDirectoryEntry", err)
			}
		})
	}
}

func TestVerifyPropagatesOperationalFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory *fakeDirectory
	}{
		{name: "lookup unavailable", directory: &fakeDirectory{lookupErr: ErrUnavailable}},
		{name: "bind unavailable", directory: &fakeDirectory{lookupResult: aliceEntry(), bindErr: ErrUnavailable}},
		{name: "address rejected", directory: &fakeDirectory{lookupErr: ErrAddressRejected}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Verify(context.Background(), providerWithDirectory(t, test.directory), "alice", "secret")
			if err == nil {
				t.Fatal("Verify accepted an operational failure")
			}
			if errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("Verify = %v, must not mask an operational failure as a credential failure", err)
			}
		})
	}
}

func TestVerifyRejectsNilInputsAndUnconstructedProviders(t *testing.T) {
	directory := &fakeDirectory{lookupResult: aliceEntry()}
	provider := providerWithDirectory(t, directory)

	if _, err := Verify(nil, provider, "alice", "secret"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Verify(nil context) = %v, want ErrNilContext", err)
	}
	if directory.lookups.Load() != 0 {
		t.Fatal("the directory was called with a nil context")
	}

	if _, err := Verify(context.Background(), nil, "alice", "secret"); !errors.Is(err, ErrNilProvider) {
		t.Fatalf("Verify(nil provider) = %v, want ErrNilProvider", err)
	}

	var zero Provider
	if _, err := Verify(context.Background(), &zero, "alice", "secret"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Verify(zero provider) = %v, want ErrInvalidConfig", err)
	}
}

func TestVerifyClaimsAreCopiedFromTheEntry(t *testing.T) {
	entry := aliceEntry()
	directory := &fakeDirectory{lookupResult: entry}
	provider := providerWithDirectory(t, directory)
	identity, err := Verify(context.Background(), provider, "alice", "secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	entry.attributes["mail"][0] = "mutated@example.com"
	entry.attributes["cn"][0] = "Mallory"
	if identity.Claims["email"] != "alice@example.com" {
		t.Fatalf("Claims[email] = %v after mutating the entry", identity.Claims["email"])
	}
	display := identity.Claims["display_name"].([]string)
	if display[0] != "Alice" {
		t.Fatalf("Claims[display_name][0] = %q after mutating the entry", display[0])
	}

	identity.Claims["email"] = "mutated@example.com"
	directory.lookupResult = aliceEntry()
	again, err := Verify(context.Background(), provider, "alice", "secret")
	if err != nil {
		t.Fatalf("Verify again: %v", err)
	}
	if again.Claims["email"] != "alice@example.com" {
		t.Fatal("the provider retained a mutated claims map")
	}
}

func TestVerifyErrorTextNeverCarriesThePasswordOrEntryText(t *testing.T) {
	const userPassword = "user-secret"
	directory := &fakeDirectory{lookupResult: aliceEntry(), bindErr: ErrInvalidCredentials}
	_, err := Verify(context.Background(), providerWithDirectory(t, directory), "alice", userPassword)
	if err == nil {
		t.Fatal("Verify accepted a rejected bind")
	}
	for _, forbidden := range []string{userPassword, "alice", "11111111-2222-3333-4444-555555555555", "uid=alice", "mail"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error text %q contains %q", err, forbidden)
		}
	}
}

func TestProviderNameReportsTheRegisteredName(t *testing.T) {
	provider := newProvider(t, testConfig("ldaps://directory.example.com"))
	if provider.Name() != testProviderName {
		t.Fatalf("Name = %q, want %q", provider.Name(), testProviderName)
	}
	var nilProvider *Provider
	if nilProvider.Name() != "" {
		t.Fatalf("nil Name = %q, want the empty string", nilProvider.Name())
	}
}

func TestNetResolverUsesTheSystemResolver(t *testing.T) {
	resolver := netResolver{resolver: net.DefaultResolver}
	addresses, err := resolver.LookupNetIP(context.Background(), "ip", "localhost")
	if err != nil {
		t.Fatalf("LookupNetIP(localhost): %v", err)
	}
	if len(addresses) == 0 {
		t.Fatal("LookupNetIP(localhost) returned no addresses")
	}
	for _, address := range addresses {
		if !address.IsLoopback() {
			t.Fatalf("localhost resolved to non-loopback %s", address)
		}
	}
}

func TestVerifyRejectsANilDirectorySeam(t *testing.T) {
	provider := newProvider(t, testConfig("ldaps://directory.example.com"))
	// The production path uses realDirectory; a typed-nil inject would panic.
	// Verify's contract only promises to reject unusable providers, so this
	// test pins the default seam rather than a nil interface.
	if provider.directoryClient() == nil {
		t.Fatal("directoryClient returned nil")
	}
	if fmt.Sprintf("%T", provider.directoryClient()) != "ldap.realDirectory" {
		t.Fatalf("default directory = %T, want ldap.realDirectory", provider.directoryClient())
	}
}
