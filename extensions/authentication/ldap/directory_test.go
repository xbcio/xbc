package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	ldapclient "github.com/go-ldap/ldap/v3"
)

// newTLSTestServer starts a directory server whose certificate covers the
// given names. implicit selects ldaps; otherwise StartTLS is offered.
func newTLSTestServer(t *testing.T, dnsNames []string, ips []net.IP, implicit bool) (*testCA, *fakeDirectoryServer) {
	t.Helper()
	ca := newTestCA(t)
	certificate := ca.issueServerCertificate(t, dnsNames, ips)
	return ca, newFakeDirectoryServer(t, &tls.Config{Certificates: []tls.Certificate{certificate}}, implicit, !implicit)
}

// serverProvider builds a provider pointing at the fake server host.
func serverProvider(t *testing.T, server *fakeDirectoryServer, host string, pool *x509.CertPool, startTLS bool) *Provider {
	t.Helper()
	scheme := "ldaps"
	if startTLS {
		scheme = "ldap"
	}
	config := testConfig(fmt.Sprintf("%s://%s:%s", scheme, host, server.port()))
	config.StartTLS = startTLS
	config.RootCAs = pool
	config.AllowPrivate = true
	return newProvider(t, config)
}

// hostnameProvider points at ldap.test and resolves it to loopback.
func hostnameProvider(t *testing.T, server *fakeDirectoryServer, pool *x509.CertPool, startTLS bool) *Provider {
	t.Helper()
	provider := serverProvider(t, server, "ldap.test", pool, startTLS)
	provider.resolver = &staticResolver{addresses: map[string][]netip.Addr{
		"ldap.test": {netip.MustParseAddr("127.0.0.1")},
	}}
	return provider
}

func TestVerifySearchesWithAnRfc4515EscapedFilter(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)

	const username = `*)(uid=*`
	identity, err := Verify(context.Background(), provider, username, "user-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.Subject != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("Subject = %q", identity.Subject)
	}

	_, searches := server.observations()
	if len(searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(searches))
	}
	search := searches[0]
	if search.base != "ou=people,dc=example,dc=com" {
		t.Fatalf("search base = %q", search.base)
	}
	if search.sizeLimit != 2 {
		t.Fatalf("size limit = %d, want 2", search.sizeLimit)
	}
	attribute, assertion := equalityFilter(t, search.filter)
	if attribute != "uid" {
		t.Fatalf("filter attribute = %q, want uid; the user input changed the filter structure", attribute)
	}
	// The filter compiler consumes RFC 4515 escapes and encodes the decoded
	// value, so the wire assertion is the literal user name inside a single
	// equality match. Without EscapeFilter, the filter text would contain a
	// second filter and go-ldap would reject it instead of searching; the
	// successful Verify above is the escaping test.
	if ldapclient.EscapeFilter(username) == username {
		t.Fatal("test input is not escaped by ldap.EscapeFilter")
	}
	if assertion != username {
		t.Fatalf("assertion value = %q, want the literal user name %q", assertion, username)
	}

	requested := make(map[string]bool, len(search.attributes))
	for _, name := range search.attributes {
		requested[strings.ToLower(name)] = true
	}
	for _, name := range []string{"entryuuid", "mail", "cn"} {
		if !requested[name] {
			t.Fatalf("search did not request %q: %v", name, search.attributes)
		}
	}
}

func TestVerifyTreatsUnknownUserAndWrongPasswordIdentically(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)

	server.entries = nil
	_, unknownErr := Verify(context.Background(), provider, "alice", "user-secret")

	server.entries = []fakeEntry{aliceServerEntry()}
	_, wrongPasswordErr := Verify(context.Background(), provider, "alice", "wrong-password")

	if !errors.Is(unknownErr, ErrInvalidCredentials) || !errors.Is(wrongPasswordErr, ErrInvalidCredentials) {
		t.Fatalf("errors = %v and %v, want ErrInvalidCredentials", unknownErr, wrongPasswordErr)
	}
	if unknownErr != ErrInvalidCredentials || wrongPasswordErr != ErrInvalidCredentials {
		t.Fatal("credential failures must be the bare sentinel with no distinguishing detail")
	}
	if unknownErr.Error() != wrongPasswordErr.Error() {
		t.Fatalf("errors differ: %q vs %q", unknownErr, wrongPasswordErr)
	}
}

func TestVerifyRejectsMultipleMatches(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	server.entries = []fakeEntry{
		aliceServerEntry(),
		{dn: "uid=bob,ou=people,dc=example,dc=com", attributes: map[string][]string{
			"uid": {"bob"}, "entryUUID": {"22222222-3333-4444-5555-666666666666"},
		}},
	}

	t.Run("two entries", func(t *testing.T) {
		_, err := Verify(context.Background(), provider, "alice", "user-secret")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Verify = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("server size limit exceeded", func(t *testing.T) {
		server.searchDoneCode = 4 // LDAPResultSizeLimitExceeded
		defer func() { server.searchDoneCode = 0 }()
		_, err := Verify(context.Background(), provider, "alice", "user-secret")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Verify = %v, want ErrInvalidCredentials", err)
		}
	})
}

func TestVerifyFailsWhenTheServiceBindIsRejected(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	server.serviceBindCode = 49

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	_, searches := server.observations()
	if len(searches) != 0 {
		t.Fatal("the search ran after the service bind failed")
	}
}

func TestVerifyTimesOutOnAHangingBind(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	provider.settings.operationTimeout = 150 * time.Millisecond
	server.hangBinds = true

	started := time.Now()
	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Verify took %s, want it bounded by the operation timeout", elapsed)
	}
}

func TestVerifyTimesOutOnAHangingSearch(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	provider.settings.operationTimeout = 150 * time.Millisecond
	server.hang = true

	started := time.Now()
	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Verify took %s, want it bounded by the operation timeout", elapsed)
	}
}

func TestVerifyHonorsContextCancellation(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	provider.settings.operationTimeout = 5 * time.Second
	server.hang = true

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Verify(ctx, provider, "alice", "user-secret")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Verify took %s after cancellation", elapsed)
	}
}

func TestVerifyReportsANonCredentialBindFailureAsUnavailable(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	server.userBindCode = 53 // unwillingToPerform

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
}

func TestVerifyUsesASeparateConnectionAndTheMatchedDistinguishedName(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)

	if _, err := Verify(context.Background(), provider, "alice", "user-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if accepted := server.acceptedCount(); accepted != 2 {
		t.Fatalf("server accepted %d connections, want 2 (service search and user bind)", accepted)
	}
	binds, _ := server.observations()
	if len(binds) != 2 {
		t.Fatalf("binds = %d, want 2", len(binds))
	}
	if binds[0].dn != "cn=xbc-search,ou=services,dc=example,dc=com" || binds[0].password != "service-secret" {
		t.Fatalf("first bind = %+v, want the service account", binds[0])
	}
	if binds[1].dn != aliceServerEntry().dn || binds[1].password != "user-secret" {
		t.Fatalf("second bind = %+v, want the matched user DN", binds[1])
	}
}

func TestVerifyAcceptsALdapsConnectionWithATrustedCA(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	identity, err := Verify(context.Background(), provider, "alice", "user-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.Subject == "" {
		t.Fatal("Verify returned an empty subject")
	}
}

func TestVerifyRejectsAnUntrustedCertificate(t *testing.T) {
	_, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, nil, false)
	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	binds, _ := server.observations()
	if len(binds) != 0 {
		t.Fatal("a bind was sent over a connection whose certificate was not trusted")
	}
}

func TestVerifyRejectsAHostnameMismatch(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"other.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)
	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	binds, _ := server.observations()
	if len(binds) != 0 {
		t.Fatal("a bind was sent over a connection with the wrong server name")
	}
}

func TestVerifyUpgradesAPlaintextConnectionWithStartTLS(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, false)
	provider := hostnameProvider(t, server, ca.pool, true)

	identity, err := Verify(context.Background(), provider, "alice", "user-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.Subject == "" {
		t.Fatal("Verify returned an empty subject")
	}
	if server.startTLSRequests.Load() != 2 {
		t.Fatalf("StartTLS requests = %d, want 2 (one per connection)", server.startTLSRequests.Load())
	}
}

func TestVerifyDoesNotFallBackToPlaintextWhenStartTLSIsRefused(t *testing.T) {
	ca := newTestCA(t)
	certificate := ca.issueServerCertificate(t, []string{"ldap.test"}, nil)
	server := newFakeDirectoryServer(t, &tls.Config{Certificates: []tls.Certificate{certificate}}, false, false)
	provider := hostnameProvider(t, server, ca.pool, true)

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
	binds, searches := server.observations()
	if len(binds) != 0 || len(searches) != 0 {
		t.Fatalf("plaintext directory operations ran after StartTLS was refused: binds=%d searches=%d", len(binds), len(searches))
	}
}

func TestTLSConfigKeepsVerificationOn(t *testing.T) {
	pool := x509.NewCertPool()
	config := testConfig("ldaps://ldap.test")
	config.RootCAs = pool
	provider := newProvider(t, config)
	tlsConfig := provider.tlsConfig()
	if tlsConfig.InsecureSkipVerify {
		t.Fatal("tls.Config disables verification")
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", tlsConfig.MinVersion)
	}
	if tlsConfig.ServerName != "ldap.test" {
		t.Fatalf("ServerName = %q", tlsConfig.ServerName)
	}
	if tlsConfig.RootCAs != pool {
		t.Fatal("RootCAs was not carried into the TLS configuration")
	}
}

func TestVerifyRejectsLoopbackByDefault(t *testing.T) {
	_, server := newTLSTestServer(t, nil, []net.IP{net.ParseIP("127.0.0.1")}, true)
	config := testConfig(fmt.Sprintf("ldaps://127.0.0.1:%s", server.port()))
	config.RootCAs = nil
	config.AllowPrivate = false
	provider := newProvider(t, config)

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("Verify = %v, want ErrAddressRejected", err)
	}
	if server.acceptedCount() != 0 {
		t.Fatal("a loopback connection was attempted under the default policy")
	}
}

func TestVerifyRejectsPrivateResolutionByDefault(t *testing.T) {
	config := testConfig("ldaps://ldap.test")
	config.AllowPrivate = false
	provider := newProvider(t, config)
	dialed := 0
	provider.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("must not dial")
	}
	provider.resolver = &staticResolver{addresses: map[string][]netip.Addr{
		"ldap.test": {netip.MustParseAddr("10.1.2.3")},
	}}

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("Verify = %v, want ErrAddressRejected", err)
	}
	if dialed != 0 {
		t.Fatal("a private address was dialed")
	}
}

func TestVerifyRejectsAMixedPublicPrivateResolution(t *testing.T) {
	config := testConfig("ldaps://ldap.test")
	config.AllowPrivate = false
	provider := newProvider(t, config)
	dialed := 0
	provider.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("must not dial")
	}
	provider.resolver = &staticResolver{addresses: map[string][]netip.Addr{
		"ldap.test": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.1.2.3")},
	}}

	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("Verify = %v, want the whole answer rejected", err)
	}
	if dialed != 0 {
		t.Fatal("one candidate of a mixed answer was dialed")
	}
}

func TestVerifyRejectsSpecialAddressesEvenWithAllowPrivate(t *testing.T) {
	for _, test := range []struct {
		name    string
		address string
	}{
		{name: "cloud metadata", address: "169.254.169.254"},
		{name: "link local ipv6", address: "fe80::1"},
		{name: "documentation range", address: "192.0.2.10"},
		{name: "multicast", address: "224.0.0.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig("ldaps://ldap.test")
			config.AllowPrivate = true
			provider := newProvider(t, config)
			dialed := 0
			provider.dialContext = func(context.Context, string, string) (net.Conn, error) {
				dialed++
				return nil, errors.New("must not dial")
			}
			provider.resolver = &staticResolver{addresses: map[string][]netip.Addr{
				"ldap.test": {netip.MustParseAddr(test.address)},
			}}

			_, err := Verify(context.Background(), provider, "alice", "user-secret")
			if !errors.Is(err, ErrAddressRejected) {
				t.Fatalf("Verify = %v, want ErrAddressRejected", err)
			}
			if dialed != 0 {
				t.Fatalf("address %s was dialed", test.address)
			}
		})
	}
}

func TestVerifyDialsTheResolvedNumericAddress(t *testing.T) {
	ca, server := newTLSTestServer(t, []string{"ldap.test"}, nil, true)
	provider := hostnameProvider(t, server, ca.pool, false)

	var mu sync.Mutex
	var dialed []string
	dial := provider.dialContext
	provider.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		return dial(ctx, network, address)
	}

	if _, err := Verify(context.Background(), provider, "alice", "user-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) == 0 {
		t.Fatal("Verify did not dial")
	}
	for _, address := range dialed {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatalf("dialed address %q: %v", address, err)
		}
		if host != "127.0.0.1" {
			t.Fatalf("dialed %q, want the validated numeric address", address)
		}
	}
}

func TestVerifyDoesNotResolveALiteralAddress(t *testing.T) {
	ca, server := newTLSTestServer(t, nil, []net.IP{net.ParseIP("127.0.0.1")}, true)
	provider := serverProvider(t, server, "127.0.0.1", ca.pool, false)
	resolver := &staticResolver{err: errors.New("literal addresses must not be resolved")}
	provider.resolver = resolver

	if _, err := Verify(context.Background(), provider, "alice", "user-secret"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resolver.calls.Load() != 0 {
		t.Fatal("the resolver was consulted for a literal address")
	}
}

func TestVerifyRejectsResolutionFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolver *staticResolver
	}{
		{name: "resolver error", resolver: &staticResolver{err: errors.New("lookup failed")}},
		{name: "empty answer", resolver: &staticResolver{addresses: map[string][]netip.Addr{"ldap.test": {}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newProvider(t, testConfig("ldaps://ldap.test"))
			provider.resolver = test.resolver
			_, err := Verify(context.Background(), provider, "alice", "user-secret")
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Verify = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestVerifyReportsDialFailuresAsUnavailable(t *testing.T) {
	config := testConfig("ldaps://ldap.test")
	provider := newProvider(t, config)
	provider.resolver = &staticResolver{addresses: map[string][]netip.Addr{
		"ldap.test": {netip.MustParseAddr("93.184.216.34")},
	}}
	provider.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	_, err := Verify(context.Background(), provider, "alice", "user-secret")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify = %v, want ErrUnavailable", err)
	}
}

// aliceServerEntry is the entry the fake server returns for a matching search.
func aliceServerEntry() fakeEntry {
	return fakeEntry{
		dn: "uid=alice,ou=people,dc=example,dc=com",
		attributes: map[string][]string{
			"uid":       {"alice"},
			"entryUUID": {"11111111-2222-3333-4444-555555555555"},
			"mail":      {"alice@example.com"},
			"cn":        {"Alice"},
		},
	}
}
