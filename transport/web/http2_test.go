package web_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// protocolObservation is what one request looked like from a PhaseObserve
// middleware: the protocol the handler chain sees and the ALPN protocol the
// handshake negotiated. Together they pin both halves of the HTTP/2 contract
// -- the wire negotiated h2, and the request reached the chain as an ordinary
// HTTP/2 request.
type protocolObservation struct {
	proto              string
	tlsPresent         bool
	negotiatedProtocol string
}

// newHTTP2Client builds a client that offers HTTP/2 over TLS.
// ForceAttemptHTTP2 is required rather than stylistic: net/http deliberately
// keeps HTTP/2 off when the caller supplied a TLSClientConfig, so without the
// flag the client would offer HTTP/1.1 alone and an "h2 enabled" test would
// prove nothing.
func newHTTP2Client(roots *x509.CertPool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots},
			ForceAttemptHTTP2: true,
		},
	}
}

// http2Config is the serving configuration these tests share: an ephemeral
// address terminating TLS with a generated pair, with HTTP/2 on or off.
func http2Config(certFile, keyFile string, enabled bool) web.Config {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile, HTTP2: enabled}
	return cfg
}

// startHTTP2Server starts a server and opens its traffic gate. newPingServer
// owns Stop and the task scope on cleanup.
func startHTTP2Server(t *testing.T, cfg web.Config, inputs serverInputs) *web.Server {
	t.Helper()

	server, ctx, host := newPingServer(t, cfg, inputs)
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	return server
}

// getEventually performs GET until the listener answers, then returns the
// response. A refused connection means "not serving yet", not a failed
// assertion; the caller asserts on what the eventual answer was.
func getEventually(t *testing.T, client *http.Client, endpoint string) *http.Response {
	t.Helper()

	var response *http.Response
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client.Get(endpoint)
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "the listener never answered %s", endpoint)
	return response
}

// TestHTTP2IsServedWhenEnabled is the enabled end-to-end case: with
// web.tls.http2 on, a client offering h2 negotiates it, and the request is an
// ordinary HTTP/2 request inside the chain -- Request().TLS is populated and
// the middleware observes "HTTP/2.0". Nothing in web or the engine advertises
// h2 except the listener's ALPN list; the standard library installs its own
// handler from there.
func TestHTTP2IsServedWhenEnabled(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)

	var (
		mu       sync.Mutex
		observed []protocolObservation
	)
	observer := fakeMiddleware{
		order: web.Order{Phase: web.PhaseObserve},
		handler: func(_ context.Context, c *web.Ctx) error {
			request := c.Request()
			observation := protocolObservation{proto: request.Proto, tlsPresent: request.TLS != nil}
			if request.TLS != nil {
				observation.negotiatedProtocol = request.TLS.NegotiatedProtocol
			}
			mu.Lock()
			observed = append(observed, observation)
			mu.Unlock()
			c.Next()
			return nil
		},
	}

	server := startHTTP2Server(t, http2Config(certFile, keyFile, true), serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{{Identity: plugin.Identity{Plugin: "http2-observer"}, Value: observer}},
	})

	client := newHTTP2Client(roots, 5*time.Second)
	t.Cleanup(client.CloseIdleConnections)

	response := getEventually(t, client, "https://"+server.Addr()+"/ping")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, response.ProtoMajor, "web.tls.http2 must negotiate HTTP/2 with a client that offers it")
	require.NotNil(t, response.TLS)
	assert.Equal(t, "h2", response.TLS.NegotiatedProtocol)

	mu.Lock()
	assert.Equal(t, []protocolObservation{{proto: "HTTP/2.0", tlsPresent: true, negotiatedProtocol: "h2"}}, observed,
		"the chain must see an ordinary HTTP/2 request with Request().TLS populated")
	mu.Unlock()
}

// TestHTTP2EnabledKeepsHTTP1Fallback is the compatibility half of the ALPN
// order: http/1.1 stays in the list, so a client that cannot speak h2 is still
// served by the same listener.
func TestHTTP2EnabledKeepsHTTP1Fallback(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	server := startHTTP2Server(t, http2Config(certFile, keyFile, true), serverInputs{})

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"http/1.1"}},
		ForceAttemptHTTP2: false,
	}}
	t.Cleanup(client.CloseIdleConnections)

	response := getEventually(t, client, "https://"+server.Addr()+"/ping")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "HTTP/1.1", response.Proto, "the fallback is what lets an existing HTTP/1.1 client keep working")
	require.NotNil(t, response.TLS)
	assert.Equal(t, "http/1.1", response.TLS.NegotiatedProtocol)
}

// TestHTTP2StaysOffByDefault pins the unchanged wire behavior: without the
// option the listener advertises http/1.1 alone. The h2-only client is the
// evidence -- a client offering both protocols would end up on http/1.1 for
// two different reasons, but a client offering only h2 can only be refused by
// a server that never advertised it.
func TestHTTP2StaysOffByDefault(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	server := startHTTP2Server(t, http2Config(certFile, keyFile, false), serverInputs{})
	addr := server.Addr()

	fallback := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}},
		ForceAttemptHTTP2: false,
	}}
	t.Cleanup(fallback.CloseIdleConnections)
	response := getEventually(t, fallback, "https://"+addr+"/ping")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "HTTP/1.1", response.Proto)
	require.NotNil(t, response.TLS)
	assert.Equal(t, "http/1.1", response.TLS.NegotiatedProtocol,
		"a client offering h2 must still be answered over http/1.1 when the option is off")

	h2Only := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"h2"}},
		ForceAttemptHTTP2: false,
	}}
	t.Cleanup(h2Only.CloseIdleConnections)
	_, err := h2Only.Get("https://" + addr + "/ping")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no application protocol",
		"the default must offer ALPN http/1.1 alone, so an h2-only client fails the handshake")
}

// TestManagementPlaneServesHTTP2 covers the second socket: the same TLS
// configuration serves both planes, so a configured management listener
// advertises h2 as well and an h2 client reaches a management route there.
func TestManagementPlaneServesHTTP2(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	cfg := http2Config(certFile, keyFile, true)
	cfg.Management.Addr = "127.0.0.1:0"
	server := startHTTP2Server(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})

	addr := server.ManagementAddr()
	require.NotEmpty(t, addr, "a configured management address must be bound")

	client := newHTTP2Client(roots, 5*time.Second)
	t.Cleanup(client.CloseIdleConnections)
	response := getEventually(t, client, "https://"+addr+"/-/metrics")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, response.ProtoMajor,
		"the management listener reuses the serving TLS config, ALPN included")
}

// TestHTTP2ShutdownFinishesInFlightStreams covers the graceful half of the h2
// shutdown contract. The standard library registers its own HTTP/2 graceful
// shutdown with the http.Server, so Engine.Shutdown sends GOAWAY, refuses new
// streams, and lets the one already inside the handler finish before Stop
// reports success.
func TestHTTP2ShutdownFinishesInFlightStreams(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	holder := plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "http2-holder"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/hold", func(_ context.Context, c *web.Ctx) error {
				enterOnce.Do(func() { close(entered) })
				<-release
				c.Status(http.StatusOK)
				return nil
			})
		}},
	}

	server := startHTTP2Server(t, http2Config(certFile, keyFile, true), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{holder},
	})
	// Registered after newPingServer's own cleanup, so it runs before it: a
	// failing assertion must not leave the held stream blocking Stop forever.
	t.Cleanup(releaseHolder)

	client := newHTTP2Client(roots, 5*time.Second)
	t.Cleanup(client.CloseIdleConnections)
	endpoint := "https://" + server.Addr()
	ping := getEventually(t, client, endpoint+"/ping")
	ping.Body.Close()
	require.Equal(t, 2, ping.ProtoMajor, "the held stream must be an h2 stream")

	type heldResult struct {
		status int
		proto  string
		err    error
	}
	held := make(chan heldResult, 1)
	go func() {
		response, err := client.Get(endpoint + "/hold")
		if err != nil {
			held <- heldResult{err: err}
			return
		}
		defer response.Body.Close()
		_, readErr := io.Copy(io.Discard, response.Body)
		held <- heldResult{status: response.StatusCode, proto: response.Proto, err: readErr}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the holding request never reached its handler")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Stop(stopCtx) }()

	// A request issued after shutdown begins must be refused. The prober is a
	// fresh client, so this asserts that the listener no longer accepts new
	// connections; the graceful half -- the established connection draining
	// rather than being cut -- is the evidence below, where Stop returns nil
	// after the held stream completed. The probe has a short timeout of its own
	// so one attempt cannot consume the poll budget.
	prober := newHTTP2Client(roots, 500*time.Millisecond)
	t.Cleanup(prober.CloseIdleConnections)
	assert.True(t, pollUntil(5*time.Second, 25*time.Millisecond, func() bool {
		response, err := prober.Get(endpoint + "/ping")
		if err != nil {
			return true
		}
		response.Body.Close()
		return false
	}), "a request opened after shutdown begins must be refused")

	releaseHolder()
	select {
	case result := <-held:
		require.NoError(t, result.err, "an in-flight stream must complete rather than be cut")
		assert.Equal(t, http.StatusOK, result.status)
		assert.Equal(t, "HTTP/2.0", result.proto)
	case <-time.After(5 * time.Second):
		t.Fatal("the held stream did not finish after release")
	}
	select {
	case err := <-stopped:
		assert.NoError(t, err, "a drain that finishes inside the budget must not report a failure")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the held stream finished")
	}
}

// TestHTTP2ShutdownDeadlineCutsBlockedStreams covers the other half: a stream
// that outlives the stop context is force-closed, Stop reports the deadline,
// and the client observes the connection going away. The handler blocks on the
// request context, so the forced close is also what ends the handler.
func TestHTTP2ShutdownDeadlineCutsBlockedStreams(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)

	entered := make(chan struct{})
	var enterOnce sync.Once
	blocked := plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "http2-blocker"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/block", func(_ context.Context, c *web.Ctx) error {
				enterOnce.Do(func() { close(entered) })
				<-c.Request().Context().Done()
				return c.Request().Context().Err()
			})
		}},
	}

	server := startHTTP2Server(t, http2Config(certFile, keyFile, true), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{blocked},
	})

	client := newHTTP2Client(roots, 10*time.Second)
	t.Cleanup(client.CloseIdleConnections)
	endpoint := "https://" + server.Addr()
	ping := getEventually(t, client, endpoint+"/ping")
	ping.Body.Close()
	require.Equal(t, 2, ping.ProtoMajor, "the blocked stream must be an h2 stream")

	requestDone := make(chan error, 1)
	go func() {
		response, err := client.Get(endpoint + "/block")
		if err != nil {
			requestDone <- err
			return
		}
		response.Body.Close()
		requestDone <- nil
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking request never reached its handler")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelStop()
	err := server.Stop(stopCtx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a drain that outlives its deadline must report the deadline, got %v", err)

	select {
	case err := <-requestDone:
		assert.Error(t, err, "the engine must force-close the connection when the deadline expires, so the stream cannot outlive Stop")
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked request did not observe the forced close")
	}
}

// readDeadlineObservation is what a handler saw while it read a stalled body
// under a read deadline it had set for itself.
type readDeadlineObservation struct {
	proto   string
	setErr  error
	readErr error
	elapsed time.Duration
	ctxErr  error
}

// TestStalledBodyUnderAReadDeadlineDiffersBetweenHTTP2AndHTTP1 pins what a read
// deadline does to a stalled client on each protocol, because the two are not
// the same and the 408 mapping depends on the part that differs.
//
// Both protocols honour http.ResponseController.SetReadDeadline through the
// writer chain and end the read at the deadline with an error that satisfies
// os.ErrDeadlineExceeded. What differs is the request context. On HTTP/1.1 the
// failed connection read makes net/http cancel it, which is the signal the
// error boundary uses to tell a stalled client from a handler's own upstream
// timeout, so the stall is a 408. On HTTP/2 the standard library only closes
// the stream's body pipe with the deadline error (stream.onReadTimeout) and
// leaves the stream, and so its context, alive. The boundary then sees the same
// error with a live request and keeps its 500.
//
// The h2 row records that outcome as it is rather than as it might be wished:
// it is the standard library's behavior, and xbc cannot tell this stall from an
// upstream deadline on that protocol without a signal the request does not
// carry. Changing the h2 status to 408 would have to be a deliberate decision
// that accepts blaming the client for upstream timeouts, and would turn this row
// red.
//
// The client never completes its body (the pipe stays open until cleanup), so
// the deadline is the only thing that can end the handler's read. The contract
// this records is documented under HTTP error contract in package web's doc
// comment.
func TestStalledBodyUnderAReadDeadlineDiffersBetweenHTTP2AndHTTP1(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)

	observed := make(chan readDeadlineObservation, 1)
	stall := plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "tls-stall"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.POST("/slow-read", func(_ context.Context, c *web.Ctx) error {
				observation := readDeadlineObservation{proto: c.Request().Proto}
				observation.setErr = http.NewResponseController(c.Writer()).SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				if observation.setErr != nil {
					observed <- observation
					return observation.setErr
				}
				started := time.Now()
				_, err := io.ReadAll(c.Request().Body)
				observation.elapsed = time.Since(started)
				observation.readErr = err
				observation.ctxErr = c.Request().Context().Err()
				observed <- observation
				return err
			})
		}},
	}
	server := startHTTP2Server(t, http2Config(certFile, keyFile, true), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{stall},
	})

	tests := []struct {
		name         string
		client       *http.Client
		proto        string
		status       int
		code         string
		ctxCancelled bool
	}{
		{
			name:   "http2",
			client: newHTTP2Client(roots, 5*time.Second),
			proto:  "HTTP/2.0", status: http.StatusInternalServerError, code: "internal_server_error",
		},
		{
			name: "http1",
			client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"http/1.1"}},
				ForceAttemptHTTP2: false,
			}},
			proto: "HTTP/1.1", status: http.StatusRequestTimeout, code: "request_timeout", ctxCancelled: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(test.client.CloseIdleConnections)
			endpoint := "https://" + server.Addr()
			ping := getEventually(t, test.client, endpoint+"/ping")
			ping.Body.Close()
			require.Equal(t, test.proto, ping.Proto, "the row must run on the protocol it names")

			// The body never completes on purpose, so a handler that answers
			// without reading it would leave the client's write loop blocked on
			// this pipe past any client timeout. The watchdog turns that into a
			// failed request instead of a hung test.
			body, writer := io.Pipe()
			watchdog := time.AfterFunc(4*time.Second, func() { _ = writer.CloseWithError(errors.New("watchdog: body still open")) })
			t.Cleanup(func() { watchdog.Stop(); _ = writer.Close() })
			go func() { _, _ = writer.Write([]byte("abc")) }()
			request, err := http.NewRequest(http.MethodPost, endpoint+"/slow-read", body)
			require.NoError(t, err)
			request.Header.Set("Content-Type", "text/plain")

			response, err := test.client.Do(request)
			require.NoError(t, err, "the server must answer instead of leaving the stalled client hanging")
			problem := decodeProblemResponse(t, response)

			var observation readDeadlineObservation
			select {
			case observation = <-observed:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler never reported what its read saw")
			}
			assert.Equal(t, test.proto, observation.proto)
			require.NoError(t, observation.setErr, "SetReadDeadline must reach the stream or connection through the writer chain")
			assert.ErrorIs(t, observation.readErr, os.ErrDeadlineExceeded,
				"the stalled read must end with the deadline error, not with EOF or a reset")
			assert.GreaterOrEqual(t, observation.elapsed, 100*time.Millisecond,
				"the read must have waited for the deadline rather than failed at once")
			assert.Less(t, observation.elapsed, 3*time.Second,
				"only the deadline can end this read, because the client never finishes its body")
			if test.ctxCancelled {
				assert.Error(t, observation.ctxErr, "a failed HTTP/1.1 connection read cancels the request context")
			} else {
				assert.NoError(t, observation.ctxErr, "an expired HTTP/2 stream read deadline leaves the request context alive")
			}

			assert.Equal(t, test.proto, response.Proto)
			assert.Equal(t, test.status, response.StatusCode)
			assert.Equal(t, test.code, problem.Properties["code"])
		})
	}
}
