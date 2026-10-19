package ldap

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"time"

	ldapclient "github.com/go-ldap/ldap/v3"
)

// hostResolver resolves a host name to every candidate address. It exists so
// tests can pin resolution to loopback without touching the network resolver.
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

// nonPublicPrefixes mirrors the address policy in
// extensions/authentication/federation/outbound, which itself mirrors the
// private policy of extensions/messaging/webhook. The federation specification
// registers the repetition deliberately: the LDAP client is the third real
// consumer, and extracting a shared package is a separate architecture
// decision. It covers special-purpose, documentation, benchmarking,
// translation, and reserved ranges that netip.Addr.IsPrivate alone does not.
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

// allowsAddress applies the policy to one resolved address. Link-local
// addresses stay rejected even when AllowPrivate is set: cloud metadata
// endpoints live there, and no directory deployment needs them.
func allowsAddress(allowPrivate bool, address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if isPublicAddress(address) {
		return true
	}
	if !allowPrivate {
		return false
	}
	return address.IsLoopback() || address.IsPrivate() || cgnatPrefix.Contains(address)
}

// resolveAddresses returns every unique candidate address for host. A literal
// IP is used without consulting the resolver, so a configured literal address
// cannot be redirected through DNS.
func resolveAddresses(ctx context.Context, resolver hostResolver, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{literal}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrUnavailable
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
		return nil, ErrUnavailable
	}
	return result, nil
}

// connect dials the directory and upgrades the connection to TLS. Every
// resolved address is validated first, and the connection is made to the
// validated numeric address, never to the host name again: a resolver that
// changes its answer between validation and connect cannot redirect the
// socket. A mixed public/private answer is rejected as a whole. TLS is
// mandatory and its configuration is built in one place, with verification
// permanently on.
func (p *Provider) connect(ctx context.Context) (*ldapclient.Conn, error) {
	addresses, err := resolveAddresses(ctx, p.resolver, p.host)
	if err != nil {
		return nil, err
	}
	// Reject the whole answer when any candidate is disallowed. Accepting only
	// the public candidates would let resolver ordering decide which policy
	// result is observed.
	for _, candidate := range addresses {
		if !allowsAddress(p.allowPrivate, candidate) {
			return nil, ErrAddressRejected
		}
	}

	for _, candidate := range addresses {
		conn, err := p.dialOne(ctx, candidate)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ErrUnavailable
			}
			continue
		}
		upgraded, err := p.upgrade(ctx, conn)
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ErrUnavailable
			}
			continue
		}
		if ctx.Err() != nil {
			_ = upgraded.Close()
			return nil, ErrUnavailable
		}
		return upgraded, nil
	}
	return nil, ErrUnavailable
}

// dialOne connects to one already-validated numeric address.
func (p *Provider) dialOne(ctx context.Context, address netip.Addr) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, p.dialTimeout)
	defer cancel()
	return p.dialContext(dialCtx, "tcp", net.JoinHostPort(address.Unmap().String(), p.port))
}

// upgrade turns a fresh TCP connection into a TLS-protected LDAP connection,
// either directly (ldaps) or through the StartTLS extended operation (ldap).
// There is no fallback to plaintext: any TLS failure closes the connection.
func (p *Provider) upgrade(ctx context.Context, conn net.Conn) (*ldapclient.Conn, error) {
	config := p.tlsConfig()
	if p.scheme == "ldaps" {
		tlsConn := tls.Client(conn, config)
		handshakeCtx, cancel := context.WithTimeout(ctx, p.operationTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			return nil, err
		}
		return p.startedConn(tlsConn, true), nil
	}

	client := ldapclient.NewConn(conn, false)
	client.Start()
	client.SetTimeout(p.operationTimeout)
	// StartTLS performs its handshake without a context, so the socket
	// deadline bounds it. Clear the deadline afterwards; request timeouts take
	// over for the bind and search that follow.
	deadline := time.Now().Add(p.operationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	if err := client.StartTLS(config); err != nil {
		_ = client.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	client.SetTimeout(p.operationTimeout)
	return client, nil
}

// startedConn starts the go-ldap reader and applies the per-request timeout.
func (p *Provider) startedConn(conn net.Conn, isTLS bool) *ldapclient.Conn {
	client := ldapclient.NewConn(conn, isTLS)
	client.Start()
	client.SetTimeout(p.operationTimeout)
	return client
}

// tlsConfig builds the only TLS configuration this module ever uses. There is
// deliberately no switch for skipping verification: a configured RootCAs pool
// can add trust anchors but never remove the chain or host-name checks.
func (p *Provider) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName: p.host,
		MinVersion: tls.VersionTLS12,
		RootCAs:    p.rootCAs,
	}
}
