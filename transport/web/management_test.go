package web_test

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// managementMarker is the body a management-plane route answers with in these
// tests. It is distinctive on purpose: with two listeners alive, a response is
// only evidence about the plane it came from when the two planes' bodies cannot
// be confused.
const managementMarker = "management-plane"

func managementRoute(_ context.Context, c *web.Ctx) error {
	c.String(http.StatusOK, managementMarker)
	return nil
}

// managementContributor registers one route through the plane view, the way an
// operator endpoint plugin does. It is the composition every test in this file
// shares: what Router.Management does with the registration is the seam under
// test, not the endpoint's own behaviour.
func managementContributor(path string) plugin.Entry[web.RouteContributor] {
	return plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "managementtest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.Management().GET(path, managementRoute).Name("management.metrics")
		}},
	}
}

// managementConfig is the two-listener composition: a serving address and a
// management address, both ephemeral so the test never picks a real port.
func managementConfig() web.Config {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Management.Addr = "127.0.0.1:0"
	return cfg
}

// fetch performs one GET over a fresh connection and reports the status code
// and body. A client per call is deliberate: a pooled connection would let a
// keep-alive from an earlier probe decide whether a later one reaches a
// listener that has just been shut down.
func fetch(addr, path string, timeout time.Duration) (int, string, error) {
	client := &http.Client{Timeout: timeout}
	response, err := client.Get("http://" + addr + path)
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

// waitForServing polls until a request is answered, which is only possible
// after the runtime traffic gate has been released and the serving task reached
// Serve.
func waitForServing(t *testing.T, addr, path string) {
	t.Helper()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		_, _, err := fetch(addr, path, 250*time.Millisecond)
		return err == nil
	}), "the listener on %s never started serving %s", addr, path)
}

// TestManagementRouteStaysOnTheServingListenerWithoutAManagementAddress pins
// the zero-difference default: with no management address configured, a plugin
// that registers through Router.Management gets exactly the route it would have
// registered directly -- served on the serving listener, with one managed task
// and no second listener. A deployment that never sets web.management.addr must
// not observe that a plugin started asking for the plane.
func TestManagementRouteStaysOnTheServingListenerWithoutAManagementAddress(t *testing.T) {
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	assert.Empty(t, server.ManagementAddr(), "no management address is configured, so there is no second listener")

	host.releaseTraffic()
	waitForServing(t, server.Addr(), "/ping")

	status, body, err := fetch(server.Addr(), "/-/metrics", 2*time.Second)
	require.NoError(t, err, "with no management listener the route must answer on the serving listener")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body)

	tasks := host.submittedTasks()
	require.Len(t, tasks, 1, "the default composition must admit exactly the serving task")
	assert.True(t, tasks[0].critical)
}

// TestManagementListenerServesItsOwnPlane is the wiring test: with an address
// configured, the route declared through the plane view is served by the second
// listener and by nothing else, while the serving listener keeps serving exactly
// what it served before. Each address is asked for the other plane's route too,
// so a wiring that registered the plane's routes on the wrong engine -- or on
// both -- fails rather than passing on the one request each listener was
// expected to answer.
func TestManagementListenerServesItsOwnPlane(t *testing.T) {
	server, ctx, host := newPingServer(t, managementConfig(), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))

	servingAddr := server.Addr()
	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, managementAddr, "a configured management address must be bound")
	require.NotEqual(t, servingAddr, managementAddr, "the planes must not share one listener")

	host.releaseTraffic()
	waitForServing(t, servingAddr, "/ping")
	waitForServing(t, managementAddr, "/-/metrics")

	status, body, err := fetch(managementAddr, "/-/metrics", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body, "the management endpoint must answer with its own handler")

	status, _, err = fetch(servingAddr, "/-/metrics", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status, "the management route must not be reachable on the serving listener")

	status, _, err = fetch(managementAddr, "/ping", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status, "the serving route must not be reachable on the management listener")

	tasks := host.submittedTasks()
	require.Len(t, tasks, 2, "each listener is one managed task")
	assert.Equal(t, tasks[0].identity, tasks[1].identity)
	assert.True(t, tasks[0].critical)
	assert.True(t, tasks[1].critical)
}

// TestManagementRoutesNeedNoAuthenticatorOnTheServingPolicy runs the whole
// serving-policy compilation against the deny-by-default configuration a
// deployment actually boots with. No authenticator is registered, so a route
// that fell to deny would fail the freeze; the management route must not, because
// no request on its chain can reach the middleware that would enforce one.
func TestManagementRoutesNeedNoAuthenticatorOnTheServingPolicy(t *testing.T) {
	cfg := managementConfig()
	cfg.Security.Default = web.SecurityDeny
	require.Equal(t, web.SecurityDeny, cfg.Security.Default, "the fail-closed default is the premise of this test")

	host := newFakeHost()
	ctx := contextFromHost(host)
	server := web.NewServer(cfg, enginetest.Factory{}, nil, nil, []plugin.Entry[web.RouteContributor]{
		managementContributor("/-/metrics"),
		{
			Identity: plugin.Identity{Plugin: "management-mount-public"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				// A management mount is a route-table row like any other, so it
				// must carry the plane flag too: recorded as a serving row, it
				// would demand an authenticator on this composition.
				router.Management().Mount("/ops", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, managementMarker)
				}))
			}},
		},
		{
			Identity: plugin.Identity{Plugin: "management-public"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/ping", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusOK); return nil }).Auth(web.Public())
			}},
		},
	}, nil, nil, nil)
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx),
		"the management route must not be compiled into the serving policy, which has no authenticator to enforce one")

	host.releaseTraffic()
	waitForServing(t, server.ManagementAddr(), "/-/metrics")

	status, _, err := fetch(server.ManagementAddr(), "/-/metrics", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)

	status, body, err := fetch(server.ManagementAddr(), "/ops/vars", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body)
}

// TestManagementListenerAnswersWhileTheServingPlaneIsSaturated states the
// reason the second listener exists as a test: a scrape must land while the
// business ceiling is fully occupied. Both management properties are asserted
// at once -- the endpoint answers, and it takes no slot in the ceiling, so a
// scrape neither displaces business work nor keeps a saturation episode open.
func TestManagementListenerAnswersWhileTheServingPlaneIsSaturated(t *testing.T) {
	const limit = 2

	state := newBlockedHandler()
	cfg := blockedConfig(t, limit)
	cfg.Management.Addr = "127.0.0.1:0"

	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{
			managementContributor("/-/metrics"),
			{
				Identity: plugin.Identity{Plugin: "management-saturation"},
				Value: fakeRouteContributor{register: func(router *web.Router) {
					router.GET("/block", state.handle)
				}},
			},
		},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	engine := testEngineOf(t, server)

	// Fill the ceiling: both admitted requests park inside the handler, and
	// nothing releases them until the scrape below has been answered. The
	// release is guarded and deferred, as in the pressure tests, so an early
	// assertion cannot leave handlers parked past this test.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(state.release) }) }
	defer release()

	var parked sync.WaitGroup
	for i := 0; i < limit; i++ {
		parked.Add(1)
		go func() {
			defer parked.Done()
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/block", nil))
		}()
	}
	require.True(t, pollUntil(2*time.Second, 5*time.Millisecond, func() bool {
		return state.inFlight.Load() == limit
	}), "the ceiling must be fully occupied before the scrape is attempted")

	// The state the scrape must survive is proven, not assumed: one more
	// business request is refused.
	refused := httptest.NewRecorder()
	engine.ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "/block", nil))
	require.Equal(t, http.StatusServiceUnavailable, refused.Code, "the serving plane must really be saturated")

	status, body, err := fetch(server.ManagementAddr(), "/-/metrics", 2*time.Second)
	require.NoError(t, err, "a saturated process must still answer its management listener")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body)
	assert.Equal(t, int64(limit), state.inFlight.Load(),
		"the scrape must not be admitted through the business ceiling it is reporting on")

	release()
	parked.Wait()
}

// TestManagementListenerDrainsAfterTheServingPlane pins the shutdown order with
// a request parked in the business plane: while the serving engine is draining
// -- its listener already closed, its request not yet answered -- the management
// listener still answers, and by the time Stop returns it does not either. An
// implementation that shut the planes down in the other order, or left the
// management engine running, fails one half of that window each.
func TestManagementListenerDrainsAfterTheServingPlane(t *testing.T) {
	state := newBlockedHandler()
	server, ctx, host := newPingServer(t, managementConfig(), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{
			managementContributor("/-/metrics"),
			{
				Identity: plugin.Identity{Plugin: "management-stop-probe"},
				Value: fakeRouteContributor{register: func(router *web.Router) {
					router.GET("/block", state.handle)
				}},
			},
		},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))

	servingAddr := server.Addr()
	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, managementAddr)
	host.releaseTraffic()

	// The parked request must travel a real connection: that is what keeps the
	// serving engine's drain waiting while the management plane is asserted.
	//
	// The release is guarded and deferred for the same reason the pressure tests
	// guard theirs: an assertion that ends this test early would otherwise leave
	// the request parked, the serving drain waiting for it, and the cleanup's
	// own Stop -- and every later test in this binary -- waiting behind that.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(state.release) }) }
	defer release()

	var parked sync.WaitGroup
	parked.Add(1)
	go func() {
		defer parked.Done()
		response, err := http.Get("http://" + servingAddr + "/block")
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	require.True(t, pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		return state.inFlight.Load() == 1
	}), "the business request must be inside the handler before Stop begins")

	stopped := make(chan error, 1)
	go func() { stopped <- server.Stop(context.Background()) }()

	// A refused business connection is the observable state "the serving drain
	// is in progress": Shutdown closes its listener before it waits for the
	// request still parked in the handler.
	require.True(t, pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		_, _, err := fetch(servingAddr, "/ping", 200*time.Millisecond)
		return err != nil
	}), "the serving listener must be closed while its drain is still waiting")
	require.Equal(t, int64(1), state.inFlight.Load(), "the parked request must still be in the handler")

	status, body, err := fetch(managementAddr, "/-/metrics", 2*time.Second)
	require.NoError(t, err, "the management listener must answer while the serving plane drains")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker, body)

	release()
	parked.Wait()
	require.NoError(t, <-stopped)

	_, _, err = fetch(managementAddr, "/-/metrics", 200*time.Millisecond)
	assert.Error(t, err, "Stop must close the management listener once the serving plane has drained")
}

// TestHostlessManagementAddressBindsLoopback pins where a hostless management
// address actually lands. The spelling ":port" is what a bare net.Listen would
// read as every interface, which on this listener would publish an
// unauthenticated operator surface to the network -- so the Server resolves it
// to loopback before listening, and the bound address is where that resolution
// becomes visible.
func TestHostlessManagementAddressBindsLoopback(t *testing.T) {
	cfg := web.Config{Addr: "127.0.0.1:0", BasePath: "/", Management: web.ManagementConfig{Addr: ":0"}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	require.NoError(t, server.Start(ctx))

	bound := server.ManagementAddr()
	require.NotEmpty(t, bound)
	managementHost, _, err := net.SplitHostPort(bound)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", managementHost,
		"a hostless management address must bind the loopback interface rather than every one")

	host.releaseTraffic()
	waitForServing(t, bound, "/-/metrics")
}

// TestMountedManagementSubtreeIsServedOnlyOnTheManagementListener covers
// Router.Mount on the plane view. A mount is a route-table registration like
// any other, so it has to record the plane it was declared on -- and the two
// engines then have to disagree about the subtree exactly as they do about
// single paths, prefix stripping included.
func TestMountedManagementSubtreeIsServedOnlyOnTheManagementListener(t *testing.T) {
	server, ctx, host := newPingServer(t, managementConfig(), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "management-mount"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.Management().Mount("/ops", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, managementMarker+" "+r.URL.Path)
				}))
			}},
		}},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	managementAddr := server.ManagementAddr()
	waitForServing(t, managementAddr, "/ops/vars")

	status, body, err := fetch(managementAddr, "/ops/vars", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, managementMarker+" /vars", body, "the mounted handler receives the path with its prefix stripped")

	status, _, err = fetch(server.Addr(), "/ops/vars", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status, "a management mount must not be reachable on the serving listener")
}

// TestPreflightDoesNotBindTheManagementAddress uses the strongest available
// evidence that the validate command binds nothing: the configured management
// address is already held by this test, so a Preflight that listened on it
// would fail with an address-in-use error instead of returning. It also proves
// the management engine the report needs is built -- and its routes registered
// -- without a socket.
func TestPreflightDoesNotBindTheManagementAddress(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	cfg := managementConfig()
	cfg.Management.Addr = held.Addr().String()
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})

	require.NoError(t, server.Preflight(ctx),
		"a Preflight that bound the configured management address would fail here with an address-in-use error")
	assert.Empty(t, server.ManagementAddr(), "Preflight must leave the Server unstarted")
	assert.Empty(t, server.Addr())
}

// TestManagementListenerTerminatesTLSWithTheSameCertificate pins the TLS half
// of the second plane: one certificate pair, one client-auth policy, and one
// reload path serve both listeners. The rotation is what makes "one path" more
// than a claim about startup -- the handshake after it is served by the newly
// written certificate, which only a shared certificate source produces.
func TestManagementListenerTerminatesTLSWithTheSameCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, certificate := writeCertificatePair(t, dir, 1)

	cfg := managementConfig()
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	client := newTLSClient(roots)
	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, managementAddr)

	var response *http.Response
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client.Get("https://" + managementAddr + "/-/metrics")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "a management listener beside a TLS serving listener must terminate TLS too")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)

	// A cleartext request is refused by the standard library, exactly as on the
	// serving listener: wrapping the second socket is what makes the
	// deployment's one client-auth policy hold on both ports.
	plaintext, err := (&http.Client{Timeout: time.Second}).Get("http://" + managementAddr + "/-/metrics")
	require.NoError(t, err)
	defer plaintext.Body.Close()
	assert.Equal(t, http.StatusBadRequest, plaintext.StatusCode,
		"a management listener terminating TLS must not serve a plaintext request")

	servingAddr := server.Addr()
	assert.Equal(t, int64(1), servedSerial(t, client, servingAddr))
	assert.Equal(t, int64(1), servedSerialAt(t, client, managementAddr, "/-/metrics"))

	_, _, rotated := writeCertificatePair(t, dir, 2)
	roots.AddCert(rotated)

	assert.Equal(t, int64(2), servedSerial(t, client, servingAddr),
		"the serving listener picks up a rotation")
	assert.Equal(t, int64(2), servedSerialAt(t, client, managementAddr, "/-/metrics"),
		"the management listener serves the rotated certificate through the same source")
}

// servedSerialAt reports which certificate the next handshake to addr is served
// on a request to path. It generalizes servedSerial, which asks /ping and so
// only fits the serving plane.
func servedSerialAt(t *testing.T, client *http.Client, addr, path string) int64 {
	t.Helper()

	response, err := client.Get("https://" + addr + path)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, response.TLS)
	require.NotEmpty(t, response.TLS.PeerCertificates)
	return response.TLS.PeerCertificates[0].SerialNumber.Int64()
}

// TestStartFailureOnTheManagementAddressReleasesTheServingListener covers the
// failure the second listener adds to the start sequence: the serving socket is
// bound first, and the management bind can still fail after it. The failed
// Start must leave neither socket behind nor admit either serving task -- the
// first is asserted by re-binding the serving port itself, which is only
// possible once that listener is really closed.
func TestStartFailureOnTheManagementAddressReleasesTheServingListener(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	servingAddr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	cfg := web.Config{
		Addr:       servingAddr,
		BasePath:   "/",
		Management: web.ManagementConfig{Addr: held.Addr().String()},
	}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})

	err = server.Start(ctx)
	require.Error(t, err, "a management address already in use must fail Start")
	assert.ErrorContains(t, err, "management address")
	assert.Empty(t, host.submittedTasks(), "a failed Start must admit no serving task")
	assert.Empty(t, server.Addr())
	assert.Empty(t, server.ManagementAddr())

	rebound, err := net.Listen("tcp", servingAddr)
	require.NoError(t, err, "the serving listener bound before the management failure must not be left open")
	require.NoError(t, rebound.Close())
}
