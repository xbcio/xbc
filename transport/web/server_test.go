package web_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

type fakeMiddleware struct {
	handler web.Handler
	order   web.Order
}

func (m fakeMiddleware) Handler() web.Handler { return m.handler }
func (m fakeMiddleware) Order() web.Order     { return m.order }

type fakeRouteContributor struct{ register func(*web.Router) }

func (f fakeRouteContributor) RegisterRoutes(router *web.Router) { f.register(router) }

type fakeRouteCatalogListener struct{ ready func(web.RouteCatalog) error }

func (f fakeRouteCatalogListener) RoutesReady(catalog web.RouteCatalog) error {
	return f.ready(catalog)
}

type serverInputs struct {
	factory        web.EngineFactory
	middlewares    []plugin.Entry[web.Middleware]
	routes         []plugin.Entry[web.RouteContributor]
	listeners      []plugin.Entry[web.RouteCatalogListener]
	authenticators []plugin.Entry[authentication.Authenticator]
	extractors     []plugin.Entry[web.CredentialExtractor]
	mappers        []plugin.Entry[web.ErrorMapper]
}

// testEngineOf type-asserts a started Server's Engine back to the neutral test
// engine so a test can dispatch requests into the assembled route tree with
// httptest. Engine is a neutral port (see transport/web/engine.go) and
// deliberately exposes no way to serve a request without a listener.
func testEngineOf(t *testing.T, s *web.Server) *enginetest.Engine {
	t.Helper()
	engine, ok := s.Engine().(*enginetest.Engine)
	require.True(t, ok, "test server was not built with enginetest.Factory")
	return engine
}

// recordingFactory captures the Options a Server derived from its Config and
// then delegates to the neutral engine.
//
// Recording is the only way left to assert on that mapping. Options is the
// entire contract between transport/web and an engine adapter, and Engine
// deliberately exposes none of it back: TrustedProxies and MaxMultipartMemory
// have no counterpart in enginetest at all, and the timeouts it does honour
// land on an unexported *http.Server. Reaching into a concrete engine to read
// them back would only re-test that engine's own constructor, which
// engines/gin covers for the engine applications actually run.
type recordingFactory struct{ options *web.Options }

func (f recordingFactory) NewEngine(options web.Options) (web.Engine, error) {
	*f.options = options
	return enginetest.Factory{}.NewEngine(options)
}

// TestStartMapsEveryConfiguredEngineSettingOntoOptions pins the whole
// Config-to-Options translation transport/web owns. Each field is given a
// distinct value so a mapping that crossed two of them over -- read timeout
// into write timeout, say -- fails instead of passing on coincidence.
func TestStartMapsEveryConfiguredEngineSettingOntoOptions(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.ReadTimeout = 11 * time.Second
	cfg.ReadHeaderTimeout = 7 * time.Second
	cfg.WriteTimeout = 29 * time.Second
	cfg.IdleTimeout = 71 * time.Second
	cfg.MaxHeaderBytes = 256 << 10
	cfg.MaxRequestBodyBytes = 2 << 20
	cfg.MaxMultipartMemory = 3 << 20

	var options web.Options
	server, ctx, _ := newPingServer(t, cfg, serverInputs{factory: recordingFactory{options: &options}})
	require.NoError(t, server.Start(ctx))

	assert.Equal(t, cfg.ReadTimeout, options.ReadTimeout)
	assert.Equal(t, cfg.ReadHeaderTimeout, options.ReadHeaderTimeout)
	assert.Equal(t, cfg.WriteTimeout, options.WriteTimeout)
	assert.Equal(t, cfg.IdleTimeout, options.IdleTimeout)
	assert.Equal(t, cfg.MaxHeaderBytes, options.MaxHeaderBytes)
	assert.Equal(t, cfg.MaxMultipartMemory, options.MaxMultipartMemory)
	assert.True(t, options.HandleMethodNotAllowed,
		"405 必须由引擎区分出来，否则方法不匹配会退化成 404，Problem Detail 也无从产生")
}

// TestTrustedProxiesAreOptIn pins the half of the trusted-proxy contract that
// belongs to transport/web: nothing is trusted unless the application asked
// for it, and what it asked for reaches the engine verbatim. What an engine
// then does with that list -- consult X-Forwarded-For or ignore it -- is the
// engine's own behaviour, pinned by TestClientIPHonoursTrustedProxies in
// engines/gin against the engine applications actually run. enginetest cannot
// stand in for that: it reports the host part of RemoteAddr unconditionally,
// so both cases below would look identical through it.
func TestTrustedProxiesAreOptIn(t *testing.T) {
	for _, test := range []struct {
		name    string
		proxies []string
	}{
		{name: "nothing trusted by default"},
		{name: "explicit proxy forwarded to the engine", proxies: []string{"127.0.0.1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := web.DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.TrustedProxies = test.proxies

			var options web.Options
			server, ctx, _ := newPingServer(t, cfg, serverInputs{factory: recordingFactory{options: &options}})
			require.NoError(t, server.Start(ctx))

			assert.Equal(t, test.proxies, options.TrustedProxies,
				"未显式配置时不得替应用信任任何代理；显式配置时必须原样交给引擎")
		})
	}
}

func TestServerReturnsProblemDetailsForRoutingAndKnownBodyOverflow(t *testing.T) {
	bodyRoute := fakeRouteContributor{register: func(router *web.Router) {
		router.POST("/body", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil })
	}}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxRequestBodyBytes = 8
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "bodytest"}, Value: bodyRoute},
		},
	})
	require.NoError(t, server.Start(ctx))

	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		status   int
		code     string
		instance string
		allow    string
	}{
		{
			name: "not found", method: http.MethodGet, path: "/missing?secret=query",
			status: http.StatusNotFound, code: "not_found", instance: "/missing",
		},
		{
			// Allow names every method the engine's matcher would accept for
			// this path, which is the engine's answer rather than the server's:
			// the test engine is ServeMux-backed and ServeMux answers HEAD with
			// the GET handler, so HEAD is genuinely allowed here. An engine
			// whose matcher keeps HEAD separate would report only GET; what the
			// server must not do is emit a 405 with no Allow at all.
			name: "method not allowed", method: http.MethodPost, path: "/ping",
			status: http.StatusMethodNotAllowed, code: "method_not_allowed", instance: "/ping",
			allow: "GET, HEAD",
		},
		{
			name: "content length exceeds limit", method: http.MethodPost, path: "/body", body: "0123456789",
			status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", instance: "/body",
		},
		{
			// The body limit is a global middleware, so it must bound an
			// unmatched path too: a limit that only applies where a route
			// happens to exist bounds nothing a stranger has to respect.
			name: "content length exceeds limit on an unmatched path", method: http.MethodPost,
			path: "/missing", body: "0123456789",
			status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", instance: "/missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			testEngineOf(t, server).ServeHTTP(response, request)

			assert.Equal(t, test.status, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, test.status, problem.Status)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.Equal(t, test.instance, problem.Instance)
			// Result().Header is the snapshot taken at commit time. The
			// recorder's live Header() would also read back an Allow set after
			// the status line, which never reaches a real client at all.
			assert.Equal(t, test.allow, response.Result().Header.Get("Allow"))
		})
	}
}

func newPingServer(t *testing.T, cfg web.Config, inputs serverInputs) (*web.Server, *plugin.Context, *fakeHost) {
	t.Helper()

	// These tests exercise server lifecycle (routing, ordering, shutdown
	// draining), not authentication policy, and none of them register an
	// authenticator. Force Security.Default to permit unconditionally --
	// not just when it is unset -- so /ping never falls to a deny-by-default
	// tier and demands an authenticator that has nothing to do with what each
	// test actually verifies. An "if unset" guard alone is not enough: a
	// caller building cfg from DefaultConfig() (server_test.go and
	// errors_test.go both do) already carries an explicit SecurityDeny, so a
	// zero-value check silently skips exactly those callers.
	cfg.Security.Default = web.SecurityPermit

	middlewares := make([]plugin.Entry[web.Middleware], 0, len(inputs.middlewares))
	middlewares = append(middlewares, inputs.middlewares...)

	routes := make([]plugin.Entry[web.RouteContributor], 0, len(inputs.routes)+1)
	routes = append(routes, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "pingtest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/ping", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusOK); return nil })
		}},
	})
	routes = append(routes, inputs.routes...)

	factory := inputs.factory
	if factory == nil {
		factory = enginetest.Factory{}
	}

	host := newFakeHost()
	ctx := contextFromHost(host)
	server := web.NewServer(cfg, factory, middlewares, inputs.mappers, routes, inputs.listeners, inputs.authenticators, inputs.extractors)
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Errorf("stopping test server: %v", err)
		}
		host.shutdown()
	})
	return server, ctx, host
}

func pollUntil(deadline, interval time.Duration, condition func() bool) bool {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

func probe(addr, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func TestServerWaitsForRuntimeTrafficGateAfterOpenTraffic(t *testing.T) {
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()
	require.NotEmpty(t, addr)

	assert.Error(t, probe(addr, "/ping", 100*time.Millisecond), "Start must bind without serving")
	require.NoError(t, server.OpenTraffic(ctx))
	assert.False(t, host.trafficReleased(), "OpenTraffic must not release the runtime-owned gate")
	assert.Error(t, probe(addr, "/ping", 100*time.Millisecond), "successful preparation must still wait for the global gate")

	host.releaseTraffic()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		return probe(addr, "/ping", 250*time.Millisecond) == nil
	}), "closing the runtime gate must let the managed serving task call Serve")

	tasks := host.submittedTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance}, tasks[0].identity)
	assert.True(t, tasks[0].critical)
}

func TestAddrReturnsRealBoundEphemeralPort(t *testing.T) {
	server, ctx, _ := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()

	assert.NotEqual(t, "127.0.0.1:0", addr)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)
}

func TestStopBeforeTrafficGateReleaseDoesNotLeakListener(t *testing.T) {
	server, ctx, _ := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()
	require.NoError(t, server.Stop(context.Background()))

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "Stop must release a listener whose serving task is still behind the gate")
	require.NoError(t, listener.Close())
}

func TestStartFailsWhenCriticalServingTaskIsRejected(t *testing.T) {
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})
	host.rejectTasks()

	err := server.Start(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "managed serving task was rejected")
	assert.Empty(t, server.Addr())
	assert.Empty(t, host.submittedTasks())
}

func TestStartAppliesContributedMiddlewareToRoutesUnderBasePath(t *testing.T) {
	var ran bool
	middleware := fakeMiddleware{
		order: web.Order{Phase: web.PhaseBusiness},
		handler: func(_ context.Context, c *web.Ctx) error {
			ran = true
			c.Next()
			return nil
		},
	}
	server, ctx, _ := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/api"}, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{
			{Identity: plugin.Identity{Plugin: "audit"}, Value: middleware},
		},
	})

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	response := httptest.NewRecorder()
	testEngineOf(t, server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ping", nil))

	assert.Equal(t, http.StatusOK, response.Code)
	assert.True(t, ran, "middleware must be installed before the base-path Router group is created")
}

// TestGlobalMiddlewareRunsOnUnmatchedRequests pins that the global chain covers
// 404 and 405 responses, not just the routes the table happens to contain.
//
// CORS, request ids, access logs, metrics, panic recovery and the request-body
// limit are all contributed as global middleware, and an unmatched request is
// still a request a client made: a chain that skips it stops bounding the body
// a stranger may POST to an arbitrary path, and makes exactly the traffic worth
// investigating the traffic nothing records.
//
// The topology is the Server's own -- the chain is spliced into the NoRoute and
// NoMethod chains by (*Server).Start. A test that assembled an engine by hand
// would be asserting a shape no Server ever builds, which is what let this
// regression through.
func TestGlobalMiddlewareRunsOnUnmatchedRequests(t *testing.T) {
	var observed []string
	middleware := fakeMiddleware{
		order: web.Order{Phase: web.PhaseObserve},
		handler: func(_ context.Context, c *web.Ctx) error {
			observed = append(observed, c.Request().Method+" "+c.Request().URL.Path)
			c.Next()
			return nil
		},
	}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{
			{Identity: plugin.Identity{Plugin: "accesslog"}, Value: middleware},
		},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))

	// A matched route is dispatched first so the assertion below distinguishes
	// "the chain never runs at all" from "the chain runs only where a route
	// matched", which is the actual regression.
	for _, test := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "matched route", method: http.MethodGet, path: "/ping", status: http.StatusOK},
		{name: "no route", method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
		{name: "method not allowed", method: http.MethodPost, path: "/ping", status: http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		testEngineOf(t, server).ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		assert.Equal(t, test.status, response.Code, "%s 的状态码不符", test.name)
	}

	assert.Equal(t, []string{"GET /ping", "GET /missing", "POST /ping"}, observed,
		"全局中间件必须覆盖 404 与 405，未匹配的请求同样是客户端发出的请求")
}

func TestOpenTrafficPropagatesRouteCatalogListenerErrorWithoutReleasingGate(t *testing.T) {
	wantErr := errors.New("swagger generation failed")
	listener := fakeRouteCatalogListener{ready: func(web.RouteCatalog) error { return wantErr }}
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{
		listeners: []plugin.Entry[web.RouteCatalogListener]{
			{Identity: plugin.Identity{Plugin: "swagger"}, Value: listener},
		},
	})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()
	err := server.OpenTraffic(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.ErrorContains(t, err, "swagger")
	assert.False(t, host.trafficReleased())
	assert.Error(t, probe(addr, "/ping", 100*time.Millisecond), "failed preparation must leave ingress blocked")
}

func TestOpenTrafficReturnsRouteFreezeErrorWithoutNotifyingListeners(t *testing.T) {
	listenerCalled := false
	invalidRoute := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/private", func(context.Context, *web.Ctx) error { return nil }).Auth(web.Accepts())
	}}
	listener := fakeRouteCatalogListener{ready: func(web.RouteCatalog) error {
		listenerCalled = true
		return nil
	}}
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "invalid-route"}, Value: invalidRoute},
		},
		listeners: []plugin.Entry[web.RouteCatalogListener]{
			{Identity: plugin.Identity{Plugin: "route-listener"}, Value: listener},
		},
	})

	require.NoError(t, server.Start(ctx))
	err := server.OpenTraffic(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "accepts no authentication schemes")
	assert.False(t, listenerCalled)
	assert.False(t, host.trafficReleased())
}

// TestOpenTrafficFailsWhenARouteFallsToDenyWithoutAuthenticator pins the
// deny-by-default promise every management plugin now relies on. metrics, pprof
// and gracefulshutdown deliberately register their endpoints without declaring
// .Auth(...), so an enabled endpoint resolves through tier-3 "default: deny";
// an application that turned one on without registering an authenticator must
// fail startup in the RoutesReady phase instead of serving it.
//
// newPingServer cannot be reused here: it forces Security.Default = permit,
// which is exactly the tier under test. RoutesReady runs only inside
// OpenTraffic, so Start alone must still succeed -- asserting that separately is
// what keeps this test honest about which phase produces the failure.
func TestOpenTrafficFailsWhenARouteFallsToDenyWithoutAuthenticator(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	require.Equal(t, web.SecurityDeny, cfg.Security.Default,
		"the factory default must stay fail-closed, otherwise this test proves nothing")

	management := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/-/metrics", func(context.Context, *web.Ctx) error { return nil }).Name("management.metrics")
	}}
	host := newFakeHost()
	ctx := contextFromHost(host)
	server := web.NewServer(
		cfg,
		enginetest.Factory{},
		nil,
		nil,
		[]plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "managementtest"},
			Value:    management,
		}},
		nil, nil, nil,
	)
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	require.NoError(t, server.Start(ctx), "route policy is validated in OpenTraffic, not in Start")

	err := server.OpenTraffic(ctx)
	require.Error(t, err, "an endpoint without an auth policy must not open traffic without an authenticator")
	assert.ErrorContains(t, err, "GET /-/metrics")
	assert.ErrorContains(t, err, "requires authentication but no authenticator is registered")
	assert.False(t, host.trafficReleased())
}

// servingPingServer starts a server, prepares it, releases the runtime gate,
// and returns the bound address once the managed serving task is answering.
// Every drain assertion needs a server that is genuinely serving traffic, not
// merely bound.
func servingPingServer(t *testing.T, inputs serverInputs) (*web.Server, string) {
	t.Helper()
	server, ctx, host := newPingServer(t, web.Config{Addr: "127.0.0.1:0", BasePath: "/"}, inputs)
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	addr := server.Addr()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		return probe(addr, "/ping", 250*time.Millisecond) == nil
	}), "the serving task never began answering after the gate was released")
	return server, addr
}

// slowRoute registers /slow, which signals when it is entered, blocks until
// release is closed, and marks completion just before it returns.
func slowRoute(entered chan<- struct{}, release <-chan struct{}, completed *atomic.Bool) plugin.Entry[web.RouteContributor] {
	var enteredOnce sync.Once
	return plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "slowtest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/slow", func(_ context.Context, c *web.Ctx) error {
				enteredOnce.Do(func() { close(entered) })
				<-release
				completed.Store(true)
				c.String(http.StatusOK, "drained")
				return nil
			})
		}},
	}
}

// TestShutdownDrainsAnInFlightRequestBeforeReturning pins the graceful half of the
// drain contract: Stop with budget to spare must not return until the request
// that was already executing has produced its response.
//
// The handler is held past the moment Stop is called and released only from a
// separate goroutine, so both discriminating assertions bite: a Stop that
// force-closed instead of draining would return in well under the hold time
// with completed still false. Asserting only the client's body would pass
// against that broken implementation too, because the test waits for the
// client either way.
func TestShutdownDrainsAnInFlightRequestBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var completed atomic.Bool
	server, addr := servingPingServer(t, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{slowRoute(entered, release, &completed)},
	})

	type response struct {
		body string
		err  error
	}
	responses := make(chan response, 1)
	go func() {
		result, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			responses <- response{err: err}
			return
		}
		defer result.Body.Close()
		body, err := io.ReadAll(result.Body)
		responses <- response{body: string(body), err: err}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow handler was never entered")
	}
	// The handler is released only after Stop is already draining. Releasing
	// it beforehand would let the request finish before Stop was even called,
	// which no implementation could fail.
	const holdFor = 150 * time.Millisecond
	go func() {
		time.Sleep(holdFor)
		close(release)
	}()

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	require.NoError(t, server.Stop(stopCtx))
	elapsed := time.Since(started)
	assert.True(t, completed.Load(),
		"Stop returned while the in-flight handler was still running")
	assert.GreaterOrEqual(t, elapsed, holdFor/2,
		"Stop returned immediately instead of waiting for the in-flight request")

	select {
	case result := <-responses:
		require.NoError(t, result.err, "the drained request must receive its response")
		assert.Equal(t, "drained", result.body)
	case <-time.After(2 * time.Second):
		t.Fatal("the drained request never completed")
	}

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "a drained Stop must still release the listener")
	require.NoError(t, listener.Close())
}

// TestShutdownPreDrainUsesTheSharedDeadline keeps the optional readiness
// propagation interval inside runtime's one shutdown budget. A Web-local timer
// that ignored ctx would delay every later Stop and violate the core contract.
func TestShutdownPreDrainUsesTheSharedDeadline(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = time.Second
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	addr := server.Addr()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		return probe(addr, "/ping", 250*time.Millisecond) == nil
	}), "the serving task never began answering after the gate was released")

	stopCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := server.Stop(stopCtx)
	elapsed := time.Since(started)

	require.NoError(t, err, "an idle server can still close cleanly after the budget expires")
	assert.ErrorIs(t, stopCtx.Err(), context.DeadlineExceeded,
		"pre-drain must observe the supplied runtime deadline")
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond,
		"pre-drain must not be skipped while its shared deadline is still live")
	assert.Less(t, elapsed, 500*time.Millisecond,
		"pre-drain must stop at the supplied runtime deadline, not wait for its configured delay")

	listener, listenErr := net.Listen("tcp", addr)
	require.NoError(t, listenErr, "a deadline-expired Stop must still force-close the listener")
	require.NoError(t, listener.Close())
}

// TestShutdownIsBoundedAndReleasesTheListenerWhenDrainingExceedsItsDeadline pins
// the other half: a handler that outlives the shutdown deadline must not make
// Stop unbounded. Stop force-closes, reports the deadline, and releases the
// port — an unbounded Stop is exactly what burns the whole shared shutdown
// budget on one plugin and starves every plugin below it.
//
// The elapsed-time bound is the discriminating assertion: a Stop that waited
// for the handler would take the full hold time, while re-binding the port
// immediately afterwards is what proves the forced close actually happened
// rather than being merely reported. Stop is called on its own goroutine so
// that a Stop which never returns at all is reported as this test's own named
// failure rather than as a package-wide go test timeout.
func TestShutdownIsBoundedAndReleasesTheListenerWhenDrainingExceedsItsDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	var completed atomic.Bool
	server, addr := servingPingServer(t, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{slowRoute(entered, release, &completed)},
	})

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result, err := http.Get("http://" + addr + "/slow")
		if err == nil {
			_, _ = io.Copy(io.Discard, result.Body)
			_ = result.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow handler was never entered")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Stop runs on its own goroutine so that a Stop which ignores its deadline
	// fails here by name instead of wedging the whole package until go test's
	// own timeout fires.
	stopped := make(chan error, 1)
	started := time.Now()
	go func() { stopped <- server.Stop(stopCtx) }()
	var err error
	select {
	case err = <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop ignored its deadline and is still draining the stuck handler")
	}
	elapsed := time.Since(started)

	require.Error(t, err, "a drain that exceeds its deadline must be reported, not swallowed")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 2*time.Second,
		"Stop must be bounded by its deadline, not by the stuck handler")
	assert.False(t, completed.Load(), "the handler is still running; Stop did not wait for it")

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "a forced Stop must still release the listener")
	require.NoError(t, listener.Close())

	releaseHandler()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the forcibly closed request never finished")
	}
}
