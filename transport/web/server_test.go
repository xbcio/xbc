package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// fakeMiddlewareProvider/fakeRouteProvider/fakeRouteCatalogConsumer are
// web's own MiddlewareProvider/RouteProvider/RouteCatalogConsumer test
// doubles -- design §9 guard #10 forbids reaching into core's internal/*
// for shared fixtures, and there is no reason to anyway: these three
// interfaces are tiny enough that a closure-backed local fake is simpler
// than importing anything.
type fakeMiddlewareProvider struct{ mws []Middleware }

func (f fakeMiddlewareProvider) Middlewares() []Middleware { return f.mws }

type fakeRouteProvider struct{ register func(r *Router) }

func (f fakeRouteProvider) RegisterRoutes(r *Router) { f.register(r) }

type fakeRouteCatalogConsumer struct{ fn func(RouteCatalog) error }

func (f fakeRouteCatalogConsumer) RoutesReady(routes RouteCatalog) error { return f.fn(routes) }

type modeLogger struct {
	log.Logger
	debug bool
}

func (l modeLogger) Enabled(level log.Level) bool {
	return l.debug && level == log.DebugLevel
}

func TestSetGinModeUsesLoggerCapabilityNotGlobalConfig(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })

	setGinMode(modeLogger{Logger: log.Nop(), debug: true})
	assert.Equal(t, gin.DebugMode, gin.Mode())

	setGinMode(modeLogger{Logger: log.Nop()})
	assert.Equal(t, gin.ReleaseMode, gin.Mode())
}

// newPingServer builds a *Server wired to a fakeHost that contributes one
// RouteProvider registering "GET /ping" -> 200, plus whatever extra
// extensions the caller supplies (e.g. a failing RouteCatalogConsumer). cfg
// defaults to an ephemeral loopback address so parallel test runs never
// collide on a fixed port.
func newPingServer(t *testing.T, cfg Config, extra ...plugin.Extension[any]) (*Server, *plugin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pingRoute := fakeRouteProvider{register: func(r *Router) {
		r.GET("/ping", func(gc *gin.Context) { gc.Status(http.StatusOK) })
	}}
	id := plugin.Identity{Plugin: "pingtest", Instance: "default"}
	extensions := append([]plugin.Extension[any]{asAny(id, pingRoute)}, extra...)

	host := newFakeHost(extensions...)
	ctx := contextFromHost(host)

	s := &Server{cfg: cfg}
	return s, ctx
}

// pollUntil retries cond every interval until it reports true or deadline
// has elapsed. The deadline is a failure ceiling, never the success
// criterion itself -- a passing test always returns well before it, and
// only a genuinely stuck implementation ever burns the whole budget.
func pollUntil(deadline time.Duration, interval time.Duration, cond func() bool) bool {
	timeout := time.After(deadline)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-timeout:
			return false
		case <-ticker.C:
		}
	}
}

// TestStartBindsPortButDoesNotServeUntilOpenTraffic is readiness test #1:
// Start must bind the listening socket and return, but a request made
// before OpenTraffic has run must not receive a response -- nothing is
// calling Accept() on that listener yet. The pre-OpenTraffic probe uses a
// short request deadline as its *failure* ceiling (it is supposed to time
// out); the post-OpenTraffic probe polls up to a separate ceiling and is
// expected to succeed well inside it -- time.Sleep is never the thing that
// decides pass/fail here.
func TestStartBindsPortButDoesNotServeUntilOpenTraffic(t *testing.T) {
	s, ctx := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/", ReadTimeout: time.Second, WriteTimeout: time.Second})

	require.NoError(t, s.Start(ctx))
	addr := s.Addr()
	require.NotEmpty(t, addr, "After Start succeeds, Addr() must read the actual bound address")

	probe := func() error {
		reqCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/ping", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	}

	assert.Error(t, probe(), "OpenTraffic hasn't run yet, listener hasn't started Accept loop, request must timeout instead of getting a response")

	require.NoError(t, s.OpenTraffic(ctx))

	ok := pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		return probe() == nil
	})
	assert.True(t, ok, "After OpenTraffic, request must successfully get a response within timeout limit")

	require.NoError(t, s.Stop(context.Background()))
}

// TestAddrReturnsRealBoundEphemeralPort is readiness test #2: when Config.Addr
// asks for an OS-assigned port ("127.0.0.1:0"), Addr() must report the exact
// address Start actually bound, not the unresolved ":0" config value.
func TestAddrReturnsRealBoundEphemeralPort(t *testing.T) {
	s, ctx := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/", ReadTimeout: time.Second, WriteTimeout: time.Second})

	require.NoError(t, s.Start(ctx))
	addr := s.Addr()

	assert.NotEqual(t, "127.0.0.1:0", addr, "Addr() must resolve to the real port, can't echo the configured :0 as is")
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port, "The port must be the one actually assigned by the operating system")

	require.NoError(t, s.Stop(context.Background()))
}

// TestStopWithoutOpenTrafficDoesNotLeakListener is readiness test #3: Start
// succeeding but OpenTraffic never running (e.g. a later plugin's Start
// fails and the application aborts before the traffic-opening barrier) must
// not leak the bound file descriptor. The proof is operational, not an
// internal-state assertion: successfully rebinding the exact same address
// right after Stop is the only thing that actually demonstrates the fd was
// released.
func TestStopWithoutOpenTrafficDoesNotLeakListener(t *testing.T) {
	s, ctx := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/", ReadTimeout: time.Second, WriteTimeout: time.Second})

	require.NoError(t, s.Start(ctx))
	addr := s.Addr()

	require.NoError(t, s.Stop(context.Background()), "When Start succeeds but never OpenTraffic, Stop must cleanly shut down")

	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err, "Listener must have been released, otherwise the same address will bind: address already in use")
	require.NoError(t, ln.Close())
}

// TestStartPropagatesMiddlewareProviderExtensionsError is readiness test #4:
// an error from plugin.Extensions[T] must abort Start and surface to the
// caller, never be silently skipped -- verified here through the shared
// InitializedPlugins failure path all three Extensions[T] calls share.
func TestStartPropagatesMiddlewareProviderExtensionsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	wantErr := errors.New("Host hasn't initialized all plugins yet")
	host := newFakeHost()
	host.initializedErr = wantErr
	ctx := contextFromHost(host)

	s := &Server{cfg: Config{Addr: "127.0.0.1:0", BasePath: "/"}}
	err := s.Start(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr, "Extensions failures must be passed through to the caller as-is, cannot be swallowed")
	assert.Empty(t, s.Addr(), "Extensions failures must return before net.Listen, the port should not be bound")
}

// TestStartAppliesContributedMiddlewareToRoutesUnderBasePath is the
// server-level counterpart of router_test.go's Group/Use ordering pin: it
// drives the exact production sequence in (*Server).Start (not a hand
// rolled newRouter call) end to end over a real HTTP request, so a future
// change that reorders Start's own steps -- e.g. moving the newRouter call
// before the middleware-collection loop -- gets caught here even though it
// would never trip router_test.go's lower-level fixtures at all.
func TestStartAppliesContributedMiddlewareToRoutesUnderBasePath(t *testing.T) {
	var ran bool
	mwID := plugin.Identity{Plugin: "audit", Instance: "default"}
	mwExt := fakeMiddlewareProvider{mws: []Middleware{{
		Name:  "audit",
		Phase: PhaseBusiness,
		Handler: func(gc *gin.Context) {
			ran = true
		},
	}}}

	s, ctx := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/api", ReadTimeout: time.Second, WriteTimeout: time.Second},
		asAny(mwID, mwExt))

	require.NoError(t, s.Start(ctx))
	require.NoError(t, s.OpenTraffic(ctx))
	defer s.Stop(context.Background())

	addr := s.Addr()
	ok := pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := http.Get("http://" + addr + "/api/ping")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	require.True(t, ok, "/api/ping must respond successfully within the timeout limit")
	assert.True(t, ran, "MiddlewareProvider contributed middleware must actually apply to routes under basePath, this is a pin test for the Start internal Use-then-Group order")
}

// TestStartPropagatesRouteCatalogConsumerError covers the other error shape
// Start must not swallow: a RouteCatalogConsumer.RoutesReady call that
// itself returns a business error, distinct from an Extensions[T] lookup
// failure.
func TestStartPropagatesRouteCatalogConsumerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	wantErr := errors.New("Swagger generation failed")
	consumer := fakeRouteCatalogConsumer{fn: func(RouteCatalog) error { return wantErr }}
	consumerID := plugin.Identity{Plugin: "swagger", Instance: "default"}

	s, ctx := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, asAny(consumerID, consumer))
	err := s.Start(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr, "Business errors from RoutesReady must surface, cannot be swallowed")
	assert.Contains(t, err.Error(), "swagger")
	assert.Empty(t, s.Addr(), "RoutesReady failure must return before net.Listen, and the port must not be bound")
}
