package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type policyFunc struct {
	url func(context.Context, *url.URL) error
	ip  func(context.Context, string, netip.Addr) error
}

func (p policyFunc) ValidateURL(ctx context.Context, target *url.URL) error {
	if p.url != nil {
		return p.url(ctx, target)
	}
	return nil
}

func (p policyFunc) ValidateIP(ctx context.Context, host string, address netip.Addr) error {
	if p.ip != nil {
		return p.ip(ctx, host, address)
	}
	return nil
}

type fakeConn struct {
	remote net.Addr
	closed atomic.Bool
}

func (*fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*fakeConn) Write(value []byte) (int, error)  { return len(value), nil }
func (*fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4zero} }
func (c *fakeConn) RemoteAddr() net.Addr           { return c.remote }
func (c *fakeConn) Close() error                   { c.closed.Store(true); return nil }
func (*fakeConn) SetDeadline(time.Time) error      { return nil }
func (*fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (*fakeConn) SetWriteDeadline(time.Time) error { return nil }

func tcpAddress(raw string, port int) *net.TCPAddr {
	address := netip.MustParseAddr(raw)
	return &net.TCPAddr{IP: net.IP(address.AsSlice()), Port: port}
}

func TestDefaultEndpointPolicyRejectsNonPublicAddresses(t *testing.T) {
	policy := DefaultEndpointPolicy{}
	for _, raw := range []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.2.1", "192.168.1.1", "198.18.0.1", "203.0.113.1",
		"224.0.0.1", "255.255.255.255", "::", "::1", "fc00::1", "fe80::1",
		"2001:db8::1", "ff02::1", "::ffff:127.0.0.1",
	} {
		t.Run(raw, func(t *testing.T) {
			err := policy.ValidateIP(context.Background(), "target.test", netip.MustParseAddr(raw))
			if !errors.Is(err, ErrEndpointRejected) {
				t.Fatalf("ValidateIP(%s) error = %v, want ErrEndpointRejected", raw, err)
			}
		})
	}
	for _, raw := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		t.Run("public-"+raw, func(t *testing.T) {
			if err := policy.ValidateIP(context.Background(), "target.test", netip.MustParseAddr(raw)); err != nil {
				t.Fatalf("ValidateIP(%s) error = %v", raw, err)
			}
		})
	}
}

func TestLiteralPrivateAndLoopbackTargetsAreRejectedBeforeTransport(t *testing.T) {
	var calls atomic.Int32
	client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil, nil, nil)
	t.Cleanup(func() { _ = client.stop(context.Background()) })
	for _, rawURL := range []string{
		"https://127.0.0.1/hook",
		"https://10.0.0.1/hook",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/hook",
		"https://[fe80::1]/hook",
	} {
		delivery := testDelivery()
		delivery.URL = rawURL
		if _, err := client.Deliver(context.Background(), delivery); !errors.Is(err, ErrEndpointRejected) {
			t.Fatalf("Deliver(%q) error = %v", rawURL, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

func TestSafeDialRejectsMixedDNSAnswersBeforeDial(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	var dials atomic.Int32
	transport := newSafeTransportWithDialer(testConfig(), resolver, DefaultEndpointPolicy{}, func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	})
	_, err := transport.DialContext(context.Background(), "tcp", "hooks.example.test:443")
	if !errors.Is(err, ErrEndpointRejected) {
		t.Fatalf("DialContext() error = %v, want ErrEndpointRejected", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("dial calls = %d, want 0", dials.Load())
	}
}

func TestSafeDialUsesValidatedNumericIPAndChecksActualPeer(t *testing.T) {
	const publicIP = "93.184.216.34"
	resolver := resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "hooks.example.test" {
			t.Fatalf("LookupNetIP(%q, %q)", network, host)
		}
		return []netip.Addr{netip.MustParseAddr(publicIP)}, nil
	})
	var dialAddress string
	var policyCalls []netip.Addr
	var mu sync.Mutex
	policy := policyFunc{ip: func(_ context.Context, host string, address netip.Addr) error {
		if host != "hooks.example.test" {
			t.Fatalf("policy host = %q", host)
		}
		mu.Lock()
		policyCalls = append(policyCalls, address)
		mu.Unlock()
		return nil
	}}
	connection := &fakeConn{remote: tcpAddress(publicIP, 443)}
	transport := newSafeTransportWithDialer(testConfig(), resolver, policy, func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("dial network = %q", network)
		}
		dialAddress = address
		return connection, nil
	})
	got, err := transport.DialContext(context.Background(), "tcp", "hooks.example.test:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	if got != connection {
		t.Fatal("DialContext returned a different connection")
	}
	if dialAddress != publicIP+":443" {
		t.Fatalf("dial address = %q, want numeric validated address", dialAddress)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(policyCalls) != 2 || policyCalls[0].String() != publicIP || policyCalls[1].String() != publicIP {
		t.Fatalf("policy calls = %v, want candidate and actual peer", policyCalls)
	}
}

func TestSafeDialRejectsPeerMismatchAndClosesConnection(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	connection := &fakeConn{remote: tcpAddress("8.8.8.8", 443)}
	transport := newSafeTransportWithDialer(testConfig(), resolver, DefaultEndpointPolicy{}, func(context.Context, string, string) (net.Conn, error) {
		return connection, nil
	})
	_, err := transport.DialContext(context.Background(), "tcp", "hooks.example.test:443")
	if !errors.Is(err, ErrEndpointRejected) {
		t.Fatalf("DialContext() error = %v, want ErrEndpointRejected", err)
	}
	if !connection.closed.Load() {
		t.Fatal("mismatched connection was not closed")
	}
}

func TestSafeDialRevalidatesDNSOnEveryNewConnection(t *testing.T) {
	var resolutions atomic.Int32
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if resolutions.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	var dials atomic.Int32
	transport := newSafeTransportWithDialer(testConfig(), resolver, DefaultEndpointPolicy{}, func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return &fakeConn{remote: tcpAddress("93.184.216.34", 443)}, nil
	})
	first, err := transport.DialContext(context.Background(), "tcp", "hooks.example.test:443")
	if err != nil {
		t.Fatalf("first DialContext() error = %v", err)
	}
	_ = first.Close()
	_, err = transport.DialContext(context.Background(), "tcp", "hooks.example.test:443")
	if !errors.Is(err, ErrEndpointRejected) {
		t.Fatalf("rebound DialContext() error = %v, want ErrEndpointRejected", err)
	}
	if resolutions.Load() != 2 || dials.Load() != 1 {
		t.Fatalf("resolutions = %d, dials = %d", resolutions.Load(), dials.Load())
	}
}

func redirectResponse(request *http.Request, location string) *http.Response {
	response := testResponse(request, http.StatusTemporaryRedirect, io.NopCloser(bytes.NewReader(nil)))
	response.Header.Set("Location", location)
	return response
}

func TestRedirectsAreRevalidatedAndCrossOriginHeadersAreStripped(t *testing.T) {
	var mu sync.Mutex
	validated := make(map[string]int)
	policy := policyFunc{url: func(_ context.Context, target *url.URL) error {
		mu.Lock()
		validated[target.Host]++
		mu.Unlock()
		return nil
	}}
	var seenSecond, seenThird http.Header
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "origin.example.test":
			return redirectResponse(request, "https://second.example.test/next"), nil
		case "second.example.test":
			switch request.URL.Path {
			case "/next":
				seenSecond = request.Header.Clone()
				return redirectResponse(request, "https://second.example.test/final"), nil
			case "/final":
				seenThird = request.Header.Clone()
				return testResponse(request, http.StatusNoContent, nil), nil
			default:
				t.Fatalf("unexpected redirect path %q", request.URL.Path)
				return nil, nil
			}
		default:
			t.Fatalf("unexpected redirect host %q", request.URL.Host)
			return nil, nil
		}
	})
	client := newClient(testConfig(), transport, nil, policy, nil)
	t.Cleanup(func() { _ = client.stop(context.Background()) })
	delivery := testDelivery()
	delivery.URL = "https://origin.example.test/start"
	delivery.Headers = map[string]string{
		"Authorization":       "Bearer secret",
		"Proxy-Authorization": "Basic secret",
		"Cookie":              "session=secret",
		"Referer":             "https://private.example/path",
		"Origin":              "https://origin.example.test",
		"X-Custom-Secret":     "secret",
	}
	result, err := client.Deliver(context.Background(), delivery)
	if err != nil || result.StatusCode != http.StatusNoContent {
		t.Fatalf("Deliver() = %#v, %v", result, err)
	}
	mu.Lock()
	if validated["second.example.test"] != 2 {
		t.Fatalf("redirect URL validations = %v", validated)
	}
	mu.Unlock()
	for hop, header := range map[string]http.Header{"second": seenSecond, "third": seenThird} {
		if header == nil {
			t.Fatalf("%s-hop headers were not captured", hop)
		}
		for _, key := range []string{
			"Authorization", "Proxy-Authorization", "Cookie", "Referer", "Origin", "X-Custom-Secret",
			HeaderDeliveryID, HeaderEvent, HeaderTimestamp, HeaderSignature,
		} {
			if value := header.Get(key); value != "" {
				t.Errorf("%s hop leaked %s=%q", hop, key, value)
			}
		}
	}
}

func TestRejectedRedirectAndHTTPSDowngradeAreNeverDialed(t *testing.T) {
	t.Run("policy rejection", func(t *testing.T) {
		policy := policyFunc{url: func(_ context.Context, target *url.URL) error {
			if target.Hostname() == "blocked.example.test" {
				return errors.New("blocked")
			}
			return nil
		}}
		var calls atomic.Int32
		client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.URL.Host != "origin.example.test" {
				t.Fatalf("redirect target was dialed: %s", request.URL)
			}
			return redirectResponse(request, "https://blocked.example.test/private"), nil
		}), nil, policy, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		delivery := testDelivery()
		delivery.URL = "https://origin.example.test/start"
		_, err := client.Deliver(context.Background(), delivery)
		if !errors.Is(err, ErrEndpointRejected) {
			t.Fatalf("Deliver() error = %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("transport calls = %d, want 1", calls.Load())
		}
	})

	t.Run("HTTPS downgrade", func(t *testing.T) {
		cfg := testConfig()
		cfg.AllowHTTP = true
		var calls atomic.Int32
		client := newClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.URL.Scheme != "https" {
				t.Fatalf("downgrade target was dialed: %s", request.URL)
			}
			return redirectResponse(request, "http://origin.example.test/insecure"), nil
		}), nil, policyFunc{}, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		delivery := testDelivery()
		delivery.URL = "https://origin.example.test/start"
		_, err := client.Deliver(context.Background(), delivery)
		if !errors.Is(err, ErrEndpointRejected) || !strings.Contains(err.Error(), "endpoint") {
			t.Fatalf("Deliver() error = %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("transport calls = %d, want 1", calls.Load())
		}
	})
}

func TestMethodChangingAndExcessiveRedirectsAreRejected(t *testing.T) {
	t.Run("method change", func(t *testing.T) {
		var calls atomic.Int32
		client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := testResponse(request, http.StatusFound, nil)
			response.Header.Set("Location", "https://origin.example.test/other")
			return response, nil
		}), nil, policyFunc{}, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		delivery := testDelivery()
		delivery.URL = "https://origin.example.test/start"
		_, err := client.Deliver(context.Background(), delivery)
		if !errors.Is(err, ErrEndpointRejected) {
			t.Fatalf("Deliver() error = %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("transport calls = %d", calls.Load())
		}
	})

	t.Run("limit", func(t *testing.T) {
		cfg := testConfig()
		cfg.MaxRedirects = 1
		var calls atomic.Int32
		client := newClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			call := calls.Add(1)
			return redirectResponse(request, "https://origin.example.test/hop"+string(rune('0'+call))), nil
		}), nil, policyFunc{}, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		delivery := testDelivery()
		delivery.URL = "https://origin.example.test/start"
		_, err := client.Deliver(context.Background(), delivery)
		if !errors.Is(err, ErrEndpointRejected) {
			t.Fatalf("Deliver() error = %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("transport calls = %d, want 2", calls.Load())
		}
	})
}
