package outbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// staticResolver returns canned answers and records how often it was consulted.
type staticResolver struct {
	addresses map[string][]netip.Addr
	err       error
	calls     int
	hosts     []string
}

func (r *staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.calls++
	r.hosts = append(r.hosts, host)
	if r.err != nil {
		return nil, r.err
	}
	return r.addresses[host], nil
}

// noDial fails the test path if a dial is attempted.
func noDial(t *testing.T) dialContextFunc {
	t.Helper()
	return func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial was attempted")
		return nil, errors.New("unreachable")
	}
}

// pipeDial returns a connected pipe end and records the requested address.
func pipeDial(requested *[]string) dialContextFunc {
	return func(_ context.Context, _, address string) (net.Conn, error) {
		*requested = append(*requested, address)
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
}

func TestDefaultPolicyValues(t *testing.T) {
	policy := DefaultPolicy()
	if policy.MaxRedirects != 5 {
		t.Fatalf("MaxRedirects = %d, want 5", policy.MaxRedirects)
	}
	if policy.DialTimeout != 5*time.Second || policy.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("handshake timeouts = %v/%v, want 5s/5s", policy.DialTimeout, policy.TLSHandshakeTimeout)
	}
	if policy.ResponseHeaderTimeout != 10*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 10s", policy.ResponseHeaderTimeout)
	}
	if policy.TotalTimeout != 30*time.Second {
		t.Fatalf("TotalTimeout = %v, want 30s", policy.TotalTimeout)
	}
	if policy.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d, want 1 MiB", policy.MaxBodyBytes)
	}
	if policy.AllowHTTP || policy.AllowPrivate {
		t.Fatal("development switches must be off by default")
	}
}

func TestPolicyNormalizedFillsDefaultsAndRejectsNegativeValues(t *testing.T) {
	normalized, err := Policy{}.normalized()
	if err != nil {
		t.Fatalf("normalized() error = %v", err)
	}
	if normalized.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("MaxBodyBytes = %d, want %d", normalized.MaxBodyBytes, DefaultMaxBodyBytes)
	}
	if normalized.MaxRedirects != 5 {
		t.Fatalf("MaxRedirects = %d, want 5", normalized.MaxRedirects)
	}

	if noRedirects, err := (Policy{MaxRedirects: NoRedirects}).normalized(); err != nil {
		t.Fatalf("normalized(NoRedirects) error = %v", err)
	} else if noRedirects.MaxRedirects != NoRedirects {
		t.Fatalf("MaxRedirects = %d, want %d", noRedirects.MaxRedirects, NoRedirects)
	}

	for name, policy := range map[string]Policy{
		"negative dial timeout":      {DialTimeout: -time.Second},
		"negative handshake timeout": {TLSHandshakeTimeout: -time.Second},
		"negative response header":   {ResponseHeaderTimeout: -time.Second},
		"negative total timeout":     {TotalTimeout: -time.Second},
		"negative body limit":        {MaxBodyBytes: -1},
	} {
		if _, err := policy.normalized(); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: normalized() error = %v, want ErrInvalidPolicy", name, err)
		}
	}
}

func TestPolicyRejectsNonPublicAddresses(t *testing.T) {
	policy := DefaultPolicy()
	for _, raw := range []string{
		"127.0.0.1", "127.255.255.254", "::1",
		"10.0.0.1", "10.255.255.254", "172.16.0.1", "172.31.255.254", "192.168.0.1",
		"169.254.1.1", "169.254.169.254", "fe80::1", "fe80::abcd",
		"100.64.0.1", "100.127.255.254",
		"fc00::1", "fdff::1",
		"0.0.0.0", "::",
		"224.0.0.1", "239.255.255.250", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:127.0.0.1",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.0.1", "240.0.0.1",
	} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q) error = %v", raw, err)
		}
		if policy.allowsAddress(address) {
			t.Fatalf("allowsAddress(%s) = true, want false", raw)
		}
	}
}

func TestPolicyAdmitsPublicAddresses(t *testing.T) {
	policy := DefaultPolicy()
	for _, raw := range []string{
		"93.184.216.34", "1.1.1.1", "8.8.8.8",
		"2606:4700:4700::1111", "2001:4860:4860::8888",
	} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q) error = %v", raw, err)
		}
		if !policy.allowsAddress(address) {
			t.Fatalf("allowsAddress(%s) = false, want true", raw)
		}
	}
}

func TestAllowPrivateAdmitsDevelopmentRanges(t *testing.T) {
	policy := Policy{AllowPrivate: true}
	for _, raw := range []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.20.0.1", "192.168.1.1",
		"100.64.0.1", "fc00::1", "fd12::1", "::ffff:192.168.1.1",
	} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q) error = %v", raw, err)
		}
		if !policy.allowsAddress(address) {
			t.Fatalf("allowsAddress(%s) = false with AllowPrivate, want true", raw)
		}
	}
}

func TestAllowPrivateStillRejectsLinkLocalAndMetadata(t *testing.T) {
	policy := Policy{AllowPrivate: true}
	for _, raw := range []string{
		"169.254.169.254", "169.254.1.1", "fe80::1", "::ffff:169.254.169.254",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "192.0.2.1",
	} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q) error = %v", raw, err)
		}
		if policy.allowsAddress(address) {
			t.Fatalf("allowsAddress(%s) = true with AllowPrivate, want false", raw)
		}
	}
}

func TestPolicyControlRevalidatesDialedAddress(t *testing.T) {
	policy := DefaultPolicy()
	if err := policy.control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("control(public) error = %v", err)
	}
	for _, address := range []string{
		"169.254.169.254:443", "127.0.0.1:443", "10.0.0.1:443", "[::1]:443",
	} {
		if err := policy.control("tcp", address, nil); !errors.Is(err, ErrAddressRejected) {
			t.Fatalf("control(%s) error = %v, want ErrAddressRejected", address, err)
		}
	}
	for _, address := range []string{"", "93.184.216.34", "not-an-address:443", "[::1]"} {
		if err := policy.control("tcp", address, nil); !errors.Is(err, ErrAddressRejected) {
			t.Fatalf("control(%q) error = %v, want ErrAddressRejected", address, err)
		}
	}

	development := Policy{AllowPrivate: true}
	if err := development.control("tcp", "127.0.0.1:8080", nil); err != nil {
		t.Fatalf("control(loopback) with AllowPrivate error = %v", err)
	}
	if err := development.control("tcp", "169.254.169.254:80", nil); !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("control(metadata) with AllowPrivate error = %v, want ErrAddressRejected", err)
	}
}

func TestPolicyValidateURL(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
		raw    string
		want   error
	}{
		{"https accepted", Policy{}, "https://idp.example.test/.well-known/openid-configuration", nil},
		{"http rejected by default", Policy{}, "http://idp.example.test/", ErrURLRejected},
		{"http admitted by development switch", Policy{AllowHTTP: true}, "http://idp.example.test/", nil},
		{"unsupported scheme", Policy{}, "ftp://idp.example.test/", ErrURLRejected},
		{"userinfo rejected", Policy{}, "https://user:secret@idp.example.test/", ErrURLRejected},
		{"fragment rejected", Policy{}, "https://idp.example.test/#fragment", ErrURLRejected},
		{"relative rejected", Policy{}, "/metadata", ErrURLRejected},
		{"scheme relative rejected", Policy{}, "//idp.example.test/path", ErrURLRejected},
		{"empty port rejected", Policy{}, "https://idp.example.test:/path", ErrURLRejected},
		{"zero port rejected", Policy{}, "https://idp.example.test:0/", ErrURLRejected},
		{"out of range port rejected", Policy{}, "https://idp.example.test:99999/", ErrURLRejected},
		{"metadata literal rejected", Policy{AllowHTTP: true}, "http://169.254.169.254/latest/meta-data", ErrAddressRejected},
		{"loopback literal rejected", Policy{AllowHTTP: true}, "http://127.0.0.1:8080/", ErrAddressRejected},
		{"private literal admitted for development", Policy{AllowHTTP: true, AllowPrivate: true}, "http://10.0.0.5:8080/", nil},
		{"public literal admitted", Policy{}, "https://93.184.216.34/", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			target, parseErr := url.Parse(testCase.raw)
			if parseErr != nil {
				t.Fatalf("url.Parse(%q) error = %v", testCase.raw, parseErr)
			}
			err := testCase.policy.validateURL(target)
			if testCase.want == nil {
				if err != nil {
					t.Fatalf("validateURL(%q) error = %v, want nil", testCase.raw, err)
				}
				return
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("validateURL(%q) error = %v, want %v", testCase.raw, err, testCase.want)
			}
		})
	}
}

func TestValidatingDialRejectsMixedAddressAnswers(t *testing.T) {
	policy := DefaultPolicy()
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"idp.example.test": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("10.0.0.1"),
		},
	}}
	dialContext := newValidatingDialContext(policy, resolver, noDial(t))
	if _, err := dialContext(context.Background(), "tcp", "idp.example.test:443"); !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("dial error = %v, want ErrAddressRejected", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}

func TestValidatingDialDialsValidatedNumericAddress(t *testing.T) {
	policy := DefaultPolicy()
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"idp.example.test": {netip.MustParseAddr("93.184.216.34")},
	}}
	var requested []string
	dialContext := newValidatingDialContext(policy, resolver, pipeDial(&requested))
	conn, err := dialContext(context.Background(), "tcp", "idp.example.test:443")
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer conn.Close()
	if len(requested) != 1 || requested[0] != "93.184.216.34:443" {
		t.Fatalf("dialed addresses = %v, want [93.184.216.34:443]", requested)
	}
}

func TestValidatingDialRejectsLiteralPrivateAddressWithoutResolving(t *testing.T) {
	policy := DefaultPolicy()
	resolver := &staticResolver{err: errors.New("resolver must not be consulted")}
	dialContext := newValidatingDialContext(policy, resolver, noDial(t))
	for _, address := range []string{"169.254.169.254:443", "10.0.0.1:443", "127.0.0.1:80"} {
		if _, err := dialContext(context.Background(), "tcp", address); !errors.Is(err, ErrAddressRejected) {
			t.Fatalf("dial(%s) error = %v, want ErrAddressRejected", address, err)
		}
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 for literal addresses", resolver.calls)
	}
}

func TestValidatingDialSkipsMismatchedAddressFamily(t *testing.T) {
	policy := DefaultPolicy()
	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"idp.example.test": {
			netip.MustParseAddr("2606:4700:4700::1111"),
			netip.MustParseAddr("93.184.216.34"),
		},
	}}
	var requested []string
	dialContext := newValidatingDialContext(policy, resolver, pipeDial(&requested))
	conn, err := dialContext(context.Background(), "tcp4", "idp.example.test:443")
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer conn.Close()
	if len(requested) != 1 || requested[0] != "93.184.216.34:443" {
		t.Fatalf("dialed addresses = %v, want only the IPv4 candidate", requested)
	}
}

func TestValidatingDialReportsResolutionFailure(t *testing.T) {
	policy := DefaultPolicy()
	resolver := &staticResolver{err: errors.New("resolver failed")}
	dialContext := newValidatingDialContext(policy, resolver, noDial(t))
	if _, err := dialContext(context.Background(), "tcp", "idp.example.test:443"); !errors.Is(err, ErrNetwork) {
		t.Fatalf("dial error = %v, want ErrNetwork", err)
	}
}
