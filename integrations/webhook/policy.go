package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type endpointError struct{ reason string }

func (e *endpointError) Error() string { return "webhook: endpoint rejected: " + e.reason }
func (e *endpointError) Unwrap() error { return ErrEndpointRejected }

var nonPublicPrefixes = []netip.Prefix{
	// IPv4 special-purpose, local, documentation, benchmarking, multicast,
	// and reserved ranges. IsPrivate alone does not cover shared address
	// space, TEST-NET, or protocol-assignment ranges.
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

	// IPv6 translation, discard, protocol-assignment, documentation, 6to4,
	// segment-routing, unique-local, link-local, and multicast ranges.
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

// DefaultEndpointPolicy permits only globally routable addresses and rejects
// local and special-use ranges for both IPv4 and IPv6.
type DefaultEndpointPolicy struct{}

func (DefaultEndpointPolicy) ValidateURL(ctx context.Context, target *url.URL) error {
	if target == nil || target.Hostname() == "" {
		return &endpointError{reason: "missing host"}
	}
	if address, err := netip.ParseAddr(target.Hostname()); err == nil {
		return DefaultEndpointPolicy{}.ValidateIP(ctx, target.Hostname(), address)
	}
	return nil
}

func (DefaultEndpointPolicy) ValidateIP(_ context.Context, _ string, address netip.Addr) error {
	if !address.IsValid() || address.Zone() != "" {
		return &endpointError{reason: "invalid address"}
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return &endpointError{reason: "non-public address"}
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return &endpointError{reason: "non-public address"}
		}
	}
	return nil
}

func validateURL(ctx context.Context, raw string, allowHTTP bool, policy EndpointPolicy) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() || target.Host == "" || target.Opaque != "" {
		return nil, &endpointError{reason: "invalid absolute URL"}
	}
	if target.User != nil {
		return nil, &endpointError{reason: "userinfo is forbidden"}
	}
	if target.Fragment != "" {
		return nil, &endpointError{reason: "fragments are forbidden"}
	}
	target.Scheme = strings.ToLower(target.Scheme)
	switch target.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, &endpointError{reason: "HTTP is disabled"}
		}
	default:
		return nil, &endpointError{reason: "unsupported scheme"}
	}
	if target.Hostname() == "" {
		return nil, &endpointError{reason: "missing host"}
	}
	if strings.HasSuffix(target.Host, ":") {
		return nil, &endpointError{reason: "invalid port"}
	}
	if port := target.Port(); port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return nil, &endpointError{reason: "invalid port"}
		}
	}
	if policy == nil {
		policy = DefaultEndpointPolicy{}
	}
	if err := policy.ValidateURL(ctx, target); err != nil {
		var rejected *endpointError
		if errors.As(err, &rejected) {
			return nil, rejected
		}
		return nil, &endpointError{reason: "deployment policy"}
	}
	return target, nil
}

func policyIP(ctx context.Context, policy EndpointPolicy, host string, address netip.Addr) error {
	if policy == nil {
		policy = DefaultEndpointPolicy{}
	}
	if err := policy.ValidateIP(ctx, host, address); err != nil {
		var rejected *endpointError
		if errors.As(err, &rejected) {
			return rejected
		}
		return &endpointError{reason: "resolved address policy"}
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func endpointWrap(reason string) error { return fmt.Errorf("%w: %s", ErrEndpointRejected, reason) }
