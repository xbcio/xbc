package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const maxResponseHeaderBytes = 1 << 20

type netResolver struct{ resolver *net.Resolver }

func (r netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return r.resolver.LookupNetIP(ctx, network, host)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func newSafeTransport(cfg Config, resolver Resolver, policy EndpointPolicy) *http.Transport {
	dialer := &net.Dialer{Timeout: cfg.RequestTimeout, KeepAlive: 30 * time.Second}
	return newSafeTransportWithDialer(cfg, resolver, policy, dialer.DialContext)
}

func newSafeTransportWithDialer(cfg Config, resolver Resolver, policy EndpointPolicy, dial dialContextFunc) *http.Transport {
	if resolver == nil {
		resolver = netResolver{resolver: net.DefaultResolver}
	}
	if policy == nil {
		policy = DefaultEndpointPolicy{}
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: cfg.RequestTimeout, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport := &http.Transport{
		Proxy:                  nil, // Proxies bypass target-IP validation and are unsafe by default.
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    cfg.RequestTimeout,
		ResponseHeaderTimeout:  cfg.RequestTimeout,
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        90 * time.Second,
		MaxIdleConns:           100,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, endpointWrap("invalid dial address")
		}

		addresses, err := resolveAddresses(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		// Reject a mixed public/private answer as a whole. Picking only the
		// public answer would let an attacker influence which policy result is
		// observed through resolver ordering and connection failures.
		for _, candidate := range addresses {
			if err := policyIP(ctx, policy, host, candidate); err != nil {
				return nil, err
			}
		}

		var attempted bool
		for _, candidate := range addresses {
			if !addressMatchesNetwork(network, candidate) {
				continue
			}
			attempted = true
			// Dial the validated numeric address, never the original hostname.
			// This closes the DNS rebinding window between validation and dial.
			conn, dialErr := dial(ctx, network, net.JoinHostPort(candidate.String(), port))
			if dialErr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			actual, actualErr := connectionRemoteIP(conn)
			if actualErr != nil || actual.Unmap() != candidate.Unmap() {
				_ = conn.Close()
				return nil, endpointWrap("dialed peer address did not match validated address")
			}
			if err := policyIP(ctx, policy, host, actual); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		}
		if !attempted {
			return nil, ErrNetwork
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNetwork
	}
	return transport
}

func resolveAddresses(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
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
			return nil, endpointWrap("resolver returned an invalid address")
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

func connectionRemoteIP(conn net.Conn) (netip.Addr, error) {
	if conn == nil || conn.RemoteAddr() == nil {
		return netip.Addr{}, errors.New("missing remote address")
	}
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		address, valid := netip.AddrFromSlice(tcp.IP)
		if !valid {
			return netip.Addr{}, errors.New("invalid remote address")
		}
		if tcp.Zone != "" {
			address = address.WithZone(tcp.Zone)
		}
		return address, nil
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(host)
}
