package outbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// NoRedirects rejects every redirect. It is the value to put in
	// Policy.MaxRedirects when an endpoint must be hit at exactly the
	// registered location, such as a token or userinfo endpoint.
	NoRedirects = -1
	// DefaultMaxBodyBytes is the response-body limit ReadBody applies when it
	// is called with a non-positive limit.
	DefaultMaxBodyBytes int64 = 1 << 20
)

var (
	// ErrInvalidPolicy reports a policy with a negative timeout or body limit.
	ErrInvalidPolicy = errors.New("federation/outbound: invalid policy")
	// ErrURLRejected reports a URL that fails scheme, shape, or address
	// validation.
	ErrURLRejected = errors.New("federation/outbound: URL rejected by policy")
	// ErrAddressRejected reports a host or dialed address outside the address
	// policy.
	ErrAddressRejected = errors.New("federation/outbound: address rejected by policy")
	// ErrNetwork reports that no validated address could be dialed.
	ErrNetwork = errors.New("federation/outbound: network request failed")
)

// Policy is the outbound policy shared by every federation protocol module.
// The zero Policy is valid and selects every default.
type Policy struct {
	// AllowPrivate admits loopback, RFC 1918, carrier-grade NAT, and
	// unique-local addresses for local development against a fake IdP. It
	// never admits link-local addresses: cloud metadata endpoints live there,
	// and no federation deployment needs them. It must stay disabled in
	// production.
	AllowPrivate bool
	// AllowHTTP admits plain HTTP. Development only; it must stay disabled in
	// production.
	AllowHTTP bool
	// MaxRedirects is the number of redirects a GET or HEAD request may
	// follow. Zero selects the default of 5. NoRedirects (or any negative
	// value) rejects every redirect.
	MaxRedirects int
	// DialTimeout bounds one connection attempt. Zero selects 5s.
	DialTimeout time.Duration
	// TLSHandshakeTimeout bounds the TLS handshake. Zero selects 5s.
	TLSHandshakeTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers. Zero selects
	// 10s.
	ResponseHeaderTimeout time.Duration
	// TotalTimeout bounds the whole request including the body read. Zero
	// selects 30s.
	TotalTimeout time.Duration
	// MaxBodyBytes is the conventional response-body limit for this policy;
	// callers pass it to ReadBody, and a JWKS fetch may pass a different limit
	// explicitly. Zero selects DefaultMaxBodyBytes.
	MaxBodyBytes int64
}

// DefaultPolicy returns the policy NewClient uses when every field is left at
// its zero value.
func DefaultPolicy() Policy {
	return Policy{
		MaxRedirects:          5,
		DialTimeout:           5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TotalTimeout:          30 * time.Second,
		MaxBodyBytes:          DefaultMaxBodyBytes,
	}
}

// normalized fills zero fields with their defaults and rejects negative
// timeouts and body limits. Negative redirect counts are retained: they mean
// "follow no redirects".
func (p Policy) normalized() (Policy, error) {
	defaults := DefaultPolicy()
	if p.MaxRedirects == 0 {
		p.MaxRedirects = defaults.MaxRedirects
	}
	if p.DialTimeout == 0 {
		p.DialTimeout = defaults.DialTimeout
	}
	if p.TLSHandshakeTimeout == 0 {
		p.TLSHandshakeTimeout = defaults.TLSHandshakeTimeout
	}
	if p.ResponseHeaderTimeout == 0 {
		p.ResponseHeaderTimeout = defaults.ResponseHeaderTimeout
	}
	if p.TotalTimeout == 0 {
		p.TotalTimeout = defaults.TotalTimeout
	}
	if p.MaxBodyBytes == 0 {
		p.MaxBodyBytes = defaults.MaxBodyBytes
	}
	if p.DialTimeout < 0 || p.TLSHandshakeTimeout < 0 || p.ResponseHeaderTimeout < 0 ||
		p.TotalTimeout < 0 || p.MaxBodyBytes < 0 {
		return Policy{}, ErrInvalidPolicy
	}
	return p, nil
}

// nonPublicPrefixes mirrors the private policy of extensions/messaging/webhook
// (the federation specification registers the duplication deliberately and
// leaves extracting a shared package to review). It covers special-purpose,
// documentation, benchmarking, translation, and reserved ranges that
// netip.Addr.IsPrivate alone does not.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// cgnatPrefix is RFC 6598 shared address space, which netip does not classify
// as private.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isPublicAddress reports whether address is globally routable and outside
// every special-use range.
func isPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// allowsAddress applies the policy to one resolved or dialed address.
func (p Policy) allowsAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if isPublicAddress(address) {
		return true
	}
	if !p.AllowPrivate {
		return false
	}
	return address.IsLoopback() || address.IsPrivate() || cgnatPrefix.Contains(address)
}

// validateURL enforces the URL rules independently of any resolution: an
// absolute URL, no userinfo or fragment, an allowed scheme and port, and an
// address policy check when the host is already a literal IP.
func (p Policy) validateURL(target *url.URL) error {
	if target == nil || !target.IsAbs() || target.Host == "" || target.Opaque != "" {
		return ErrURLRejected
	}
	if target.User != nil {
		return ErrURLRejected
	}
	if target.Fragment != "" {
		return ErrURLRejected
	}
	switch strings.ToLower(target.Scheme) {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return ErrURLRejected
		}
	default:
		return ErrURLRejected
	}
	host := target.Hostname()
	if host == "" || strings.HasSuffix(target.Host, ":") {
		return ErrURLRejected
	}
	if port := target.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ErrURLRejected
		}
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !p.allowsAddress(address) {
			return ErrAddressRejected
		}
	}
	return nil
}

// control is installed as net.Dialer.Control. It re-validates the address the
// socket is about to connect to, closing the window between resolution-time
// validation and the actual connect.
func (p Policy) control(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrAddressRejected
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil || !p.allowsAddress(parsed) {
		return ErrAddressRejected
	}
	return nil
}

// hostResolver resolves a host name to every candidate address.
type hostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// netResolver adapts the standard library resolver.
type netResolver struct{ resolver *net.Resolver }

func (r netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return r.resolver.LookupNetIP(ctx, network, host)
}

// dialContextFunc matches net.Dialer.DialContext.
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// newValidatingDialContext resolves the target, rejects the whole answer when
// any candidate is not allowed, and dials a validated numeric address instead
// of the host name so the operating system cannot resolve it a second time.
func newValidatingDialContext(policy Policy, resolver hostResolver, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, ErrAddressRejected
		}
		addresses, err := resolveAddresses(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		// Reject a mixed public/private answer as a whole. Accepting only the
		// public candidates would let an attacker influence which policy result
		// is observed through resolver ordering and connection failures.
		for _, candidate := range addresses {
			if !policy.allowsAddress(candidate) {
				return nil, ErrAddressRejected
			}
		}
		for _, candidate := range addresses {
			if !addressMatchesNetwork(network, candidate) {
				continue
			}
			conn, dialErr := dial(ctx, network, net.JoinHostPort(candidate.String(), port))
			if dialErr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNetwork
	}
}

// resolveAddresses returns every unique candidate address for host, using the
// resolver only when host is not already a literal IP.
func resolveAddresses(ctx context.Context, resolver hostResolver, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{literal}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNetwork
	}
	result := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		if !address.IsValid() {
			return nil, ErrAddressRejected
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	if len(result) == 0 {
		return nil, ErrNetwork
	}
	return result, nil
}

// addressMatchesNetwork reports whether a candidate is usable on network.
func addressMatchesNetwork(network string, address netip.Addr) bool {
	switch network {
	case "tcp4":
		return address.Unmap().Is4()
	case "tcp6":
		return address.Is6() && !address.Is4In6()
	default:
		return strings.HasPrefix(network, "tcp")
	}
}
