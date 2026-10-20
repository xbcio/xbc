package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// recordingListenerProvider is the provider every test in this file drives. It
// records each address Start asks for and each listener it hands back, then
// delegates to listen, so a test can supply a real Unix socket, a real TCP
// socket, or one of the failure shapes the contract names.
type recordingListenerProvider struct {
	mu     sync.Mutex
	asked  []string
	bound  []net.Listener
	listen func(address string) (net.Listener, error)
}

func (p *recordingListenerProvider) Listen(address string) (net.Listener, error) {
	p.mu.Lock()
	p.asked = append(p.asked, address)
	p.mu.Unlock()

	ln, err := p.listen(address)
	if ln != nil {
		p.mu.Lock()
		p.bound = append(p.bound, ln)
		p.mu.Unlock()
	}
	return ln, err
}

func (p *recordingListenerProvider) addresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

func (p *recordingListenerProvider) listeners() []net.Listener {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]net.Listener(nil), p.bound...)
}

// newUnixSocketListenerProvider ignores the advisory address and binds a fresh
// Unix domain socket per call. The directory is created short rather than with
// t.TempDir: a sun_path is bounded (about 104 bytes on macOS), and a test name
// spelled into the directory could push the socket path past that bound.
func newUnixSocketListenerProvider(t *testing.T) *recordingListenerProvider {
	t.Helper()

	dir, err := os.MkdirTemp("", "xcbsock")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var count atomic.Int32
	return &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", count.Add(1))))
	}}
}

// fetchOverUnix issues one GET over a connection dialled straight to the Unix
// socket. The URL's host is irrelevant: the dialer ignores it.
func fetchOverUnix(socket, path string, timeout time.Duration) (int, string, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
	defer client.CloseIdleConnections()

	response, err := client.Get("http://unix" + path)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", err
	}
	return response.StatusCode, string(body), nil
}

func waitForServingOverUnix(t *testing.T, socket, path string) {
	t.Helper()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		_, _, err := fetchOverUnix(socket, path, 250*time.Millisecond)
		return err == nil
	}), "the listener on %s never started serving %s", socket, path)
}

// providerEntry wraps the value the way the Definition stores it, identity
// included: every failure the server reports has to name that identity.
func providerEntry(name string, provider web.ListenerProvider) plugin.Entry[web.ListenerProvider] {
	return plugin.Entry[web.ListenerProvider]{
		Identity: plugin.Identity{Plugin: plugin.Key(name)},
		Value:    provider,
	}
}

// TestListenerProviderServesAClientOverAUnixSocket is the acceptance case from
// the provider contract: a real Start/OpenTraffic/releaseTraffic cycle through a
// provider that binds a Unix domain socket, a real HTTP exchange over it, and
// the bound address reported as the socket path.
func TestListenerProviderServesAClientOverAUnixSocket(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	provider := newUnixSocketListenerProvider(t)
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("unix-socket", provider))

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	socket := server.Addr()
	require.NotEmpty(t, socket)
	assert.Equal(t, []string{cfg.Addr}, provider.addresses(),
		"the serving plane must ask the provider once, with the configured address")

	waitForServingOverUnix(t, socket, "/ping")
	status, _, err := fetchOverUnix(socket, "/ping", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
}

// TestListenerProviderListenerIsWrappedWithTLS pins the ordering the acceptance
// names: the provider produces the socket and TLS wraps above it, so a provider
// neither knows about TLS nor displaces it.
func TestListenerProviderListenerIsWrappedWithTLS(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	provider := &recordingListenerProvider{listen: func(address string) (net.Listener, error) {
		return net.Listen("tcp", address)
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("tls-provider", provider))

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	client := newTLSClient(roots)
	client.Timeout = 2 * time.Second
	var response *http.Response
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client.Get("https://" + server.Addr() + "/ping")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "a provider-bound listener must still terminate TLS")

	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, response.TLS)
	assert.Equal(t, "http/1.1", response.TLS.NegotiatedProtocol,
		"TLS terminates above the provider, and ALPN keeps advertising http/1.1")
	assert.Equal(t, []string{cfg.Addr}, provider.addresses(),
		"the provider must be asked with the configured address even when TLS wraps its listener")
}

// TestListenerProviderSuppliesBothPlanes pins that one provider serves both
// configured planes: two calls with the two configured addresses, two distinct
// sockets, and a management request answered by the second one.
func TestListenerProviderSuppliesBothPlanes(t *testing.T) {
	cfg := web.Config{
		Addr:       "127.0.0.1:0",
		BasePath:   "/",
		Management: web.ManagementConfig{Addr: "localhost:0"},
	}
	provider := newUnixSocketListenerProvider(t)
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	server.SetListenerProvider(providerEntry("unix-socket", provider))

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	servingAddr := server.Addr()
	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, servingAddr)
	require.NotEmpty(t, managementAddr)
	assert.NotEqual(t, servingAddr, managementAddr, "the two planes must not share one socket")
	assert.Equal(t, []string{cfg.Addr, cfg.Management.Addr}, provider.addresses(),
		"each plane must ask the provider with its own configured address")

	waitForServingOverUnix(t, managementAddr, "/-/metrics")
	status, body, err := fetchOverUnix(managementAddr, "/-/metrics", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body, "the management plane's own route must answer over its socket")

	status, _, err = fetchOverUnix(servingAddr, "/-/metrics", 2*time.Second)
	require.NoError(t, err, "the serving socket must answer even when the route is unmatched there")
	assert.Equal(t, http.StatusNotFound, status, "the management route must not be reachable on the serving socket")
}

// TestPreflightDoesNotCallTheListenerProvider pins the bind-free half of the
// contract: a validate run assembles and prints the report without asking the
// provider for anything, while the Start that follows asks exactly once.
func TestPreflightDoesNotCallTheListenerProvider(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	provider := &recordingListenerProvider{listen: func(address string) (net.Listener, error) {
		return net.Listen("tcp", address)
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("tcp-provider", provider))
	capture := &captureLogger{Logger: corelog.Nop()}
	host.logger = capture

	require.NoError(t, server.Preflight(ctx))
	assert.Empty(t, provider.addresses(), "Preflight must never ask the provider for a listener")
	assert.Empty(t, server.Addr(), "Preflight must leave the Server unstarted")
	assert.Contains(t, strings.Join(capture.infos(), "\n"), "web: route table (1)",
		"the validate report must still be printed without binding anything")

	require.NoError(t, server.Start(ctx))
	assert.Len(t, provider.addresses(), 1, "the following Start must ask the provider once")
}

// TestListenerProviderErrorNamesTheProviderAndAddress pins the failure shape:
// the abort carries the provider identity and the address, and nothing serves.
func TestListenerProviderErrorNamesTheProviderAndAddress(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return nil, errors.New("boom")
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("unix-socket", provider))

	err := server.Start(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listener provider unix-socket failed to listen on "+cfg.Addr)
	assert.ErrorContains(t, err, "boom")
	assert.Empty(t, server.Addr(), "a failed provider must leave no bound address")
	assert.Empty(t, host.submittedTasks(), "a failed provider must admit no serving task")
}

// TestListenerProviderListenerReturnedBesideAnErrorIsClosed pins the ownership
// rule for the ambiguous return: nothing is served and the socket is not leaked.
func TestListenerProviderListenerReturnedBesideAnErrorIsClosed(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	tracked := &closeTrackingListener{}
	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return tracked, errors.New("half bound")
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("half-bound", provider))

	err := server.Start(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listener provider half-bound")
	assert.ErrorContains(t, err, "half bound")
	assert.True(t, tracked.closed.Load(), "a listener returned beside an error must be closed by the server")
	assert.Empty(t, host.submittedTasks())
}

// TestListenerProviderNilListenerIsRefused pins the nil guard: (nil, nil) fails
// with its own message naming the provider and address instead of surfacing
// later as a nil dereference.
func TestListenerProviderNilListenerIsRefused(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return nil, nil
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("empty-handed", provider))

	err := server.Start(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listener provider empty-handed returned a nil listener for "+cfg.Addr)
	assert.Empty(t, server.Addr())
	assert.Empty(t, host.submittedTasks())
}

// TestListenerProviderTypedNilListenerIsRefused pins the typed-nil half of the
// nil guard: a non-nil interface holding a nil pointer passes ln == nil and
// would panic in the serving goroutine after Start had already reported
// success, so it is refused with the same message as a plain nil.
func TestListenerProviderTypedNilListenerIsRefused(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	var typedNil *closeTrackingListener

	t.Run("without an error", func(t *testing.T) {
		provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
			return typedNil, nil
		}}
		server, ctx, host := newPingServer(t, cfg, serverInputs{})
		server.SetListenerProvider(providerEntry("typed-nil", provider))

		err := server.Start(ctx)
		require.Error(t, err)
		assert.ErrorContains(t, err, "listener provider typed-nil returned a nil listener for "+cfg.Addr)
		assert.Empty(t, server.Addr(), "a typed-nil listener must leave no bound address")
		assert.Empty(t, host.submittedTasks(), "a typed-nil listener must admit no serving task")
	})

	t.Run("with an error", func(t *testing.T) {
		provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
			return typedNil, errors.New("synthetic failure")
		}}
		server, ctx, _ := newPingServer(t, cfg, serverInputs{})
		server.SetListenerProvider(providerEntry("typed-nil", provider))

		err := server.Start(ctx)
		require.Error(t, err)
		assert.ErrorContains(t, err, "listener provider typed-nil failed to listen on "+cfg.Addr)
		assert.ErrorContains(t, err, "synthetic failure")
	})
}

// TestListenerProviderFailureOnTheManagementPlaneClosesTheServingListener pins
// the rollback the second plane adds: the serving socket is already bound when
// the management provider call fails, and Start must close it before returning.
func TestListenerProviderFailureOnTheManagementPlaneClosesTheServingListener(t *testing.T) {
	cfg := web.Config{
		Addr:       "127.0.0.1:0",
		BasePath:   "/",
		Management: web.ManagementConfig{Addr: "localhost:0"},
	}
	var calls atomic.Int32
	unix := newUnixSocketListenerProvider(t)
	provider := &recordingListenerProvider{listen: func(address string) (net.Listener, error) {
		if calls.Add(1) == 1 {
			return unix.Listen(address)
		}
		return nil, errors.New("management socket unavailable")
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("two-plane", provider))

	err := server.Start(ctx)
	require.Error(t, err, "the management provider failure must fail Start")
	assert.ErrorContains(t, err, "listener provider two-plane")
	assert.ErrorContains(t, err, "management socket unavailable")

	bound := provider.listeners()
	require.Len(t, bound, 1, "the serving listener must have been bound before the management failure")
	assert.ErrorIs(t, bound[0].Close(), net.ErrClosed,
		"the already-bound serving listener must be closed when the management plane fails")
	assert.Empty(t, host.submittedTasks())
}

// TestServerBindsTCPWhenNoListenerProviderIsSelected is the explicit default:
// with no provider selected, Start still binds web.addr over TCP and serves it.
func TestServerBindsTCPWhenNoListenerProviderIsSelected(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/"}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err, "the default path must report a TCP host:port address")
	assert.NotEqual(t, "0", port)

	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	waitForServing(t, addr, "/ping")
}

// closeTrackingListener is a listener the server never serves from; it only
// records whether the close-on-error path reached it.
type closeTrackingListener struct{ closed atomic.Bool }

func (l *closeTrackingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *closeTrackingListener) Close() error {
	l.closed.Store(true)
	return nil
}
func (l *closeTrackingListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
