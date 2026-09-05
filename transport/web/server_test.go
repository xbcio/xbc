package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type fakeMiddleware struct {
	handler gin.HandlerFunc
	order   Order
}

func (m fakeMiddleware) Handler() gin.HandlerFunc { return m.handler }
func (m fakeMiddleware) Order() Order             { return m.order }

type fakeRouteContributor struct{ register func(*Router) }

func (f fakeRouteContributor) RegisterRoutes(router *Router) { f.register(router) }

type fakeRouteCatalogListener struct{ ready func(RouteCatalog) error }

func (f fakeRouteCatalogListener) RoutesReady(catalog RouteCatalog) error { return f.ready(catalog) }

type modeLogger struct {
	log.Logger
	debug bool
}

func (l modeLogger) Enabled(level log.Level) bool {
	return l.debug && level == log.DebugLevel
}

type serverInputs struct {
	middlewares []plugin.Entry[Middleware]
	routes      []plugin.Entry[RouteContributor]
	listeners   []plugin.Entry[RouteCatalogListener]
	mappers     []ErrorMapper
}

func TestSetGinModeUsesLoggerCapabilityNotGlobalConfig(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })

	setGinMode(modeLogger{Logger: log.Nop(), debug: true})
	assert.Equal(t, gin.DebugMode, gin.Mode())

	setGinMode(modeLogger{Logger: log.Nop()})
	assert.Equal(t, gin.ReleaseMode, gin.Mode())
}

func TestStartAppliesProductionHTTPServerSettings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.ReadTimeout = 11 * time.Second
	cfg.ReadHeaderTimeout = 7 * time.Second
	cfg.WriteTimeout = 29 * time.Second
	cfg.IdleTimeout = 71 * time.Second
	cfg.MaxHeaderBytes = 256 << 10
	cfg.MaxRequestBodyBytes = 2 << 20
	cfg.MaxMultipartMemory = 3 << 20

	server, ctx, _ := newPingServer(t, cfg, serverInputs{})
	require.NoError(t, server.Start(ctx))

	require.NotNil(t, server.srv)
	assert.Equal(t, cfg.ReadTimeout, server.srv.ReadTimeout)
	assert.Equal(t, cfg.ReadHeaderTimeout, server.srv.ReadHeaderTimeout)
	assert.Equal(t, cfg.WriteTimeout, server.srv.WriteTimeout)
	assert.Equal(t, cfg.IdleTimeout, server.srv.IdleTimeout)
	assert.Equal(t, cfg.MaxHeaderBytes, server.srv.MaxHeaderBytes)
	assert.Equal(t, cfg.MaxMultipartMemory, server.engine.MaxMultipartMemory)
}

func TestTrustedProxiesAreOptIn(t *testing.T) {
	clientIPRoute := fakeRouteContributor{register: func(router *Router) {
		router.GET("/client-ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
	}}
	clientIPEntry := plugin.Entry[RouteContributor]{
		Identity: plugin.Identity{Plugin: "clientiptest"},
		Value:    clientIPRoute,
	}

	tests := []struct {
		name    string
		proxies []string
		want    string
	}{
		{name: "forwarded header ignored by default", want: "127.0.0.1"},
		{name: "explicit proxy accepted", proxies: []string{"127.0.0.1"}, want: "203.0.113.9"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.TrustedProxies = test.proxies
			server, ctx, _ := newPingServer(t, cfg, serverInputs{
				routes: []plugin.Entry[RouteContributor]{clientIPEntry},
			})
			require.NoError(t, server.Start(ctx))

			request := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
			request.RemoteAddr = "127.0.0.1:4321"
			request.Header.Set("X-Forwarded-For", "203.0.113.9")
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, test.want, response.Body.String())
		})
	}
}

func TestServerReturnsProblemDetailsForRoutingAndKnownBodyOverflow(t *testing.T) {
	bodyRoute := fakeRouteContributor{register: func(router *Router) {
		router.POST("/body", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	}}
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxRequestBodyBytes = 8
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[RouteContributor]{
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
			name: "method not allowed", method: http.MethodPost, path: "/ping",
			status: http.StatusMethodNotAllowed, code: "method_not_allowed", instance: "/ping", allow: http.MethodGet,
		},
		{
			name: "content length exceeds limit", method: http.MethodPost, path: "/body", body: "0123456789",
			status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", instance: "/body",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)

			assert.Equal(t, test.status, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, test.status, problem.Status)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.Equal(t, test.instance, problem.Instance)
			assert.Equal(t, test.allow, response.Header().Get("Allow"))
		})
	}
}

func newPingServer(t *testing.T, cfg Config, inputs serverInputs) (*Server, *plugin.Context, *fakeHost) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	middlewares := make([]plugin.Entry[Middleware], 0, len(inputs.middlewares)+1)
	middlewares = append(middlewares, plugin.Entry[Middleware]{
		Identity: plugin.Identity{Plugin: ErrorBoundaryKey},
		Value:    &errorBoundary{mappers: append([]ErrorMapper(nil), inputs.mappers...)},
	})
	middlewares = append(middlewares, inputs.middlewares...)

	routes := make([]plugin.Entry[RouteContributor], 0, len(inputs.routes)+1)
	routes = append(routes, plugin.Entry[RouteContributor]{
		Identity: plugin.Identity{Plugin: "pingtest"},
		Value: fakeRouteContributor{register: func(router *Router) {
			router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
		}},
	})
	routes = append(routes, inputs.routes...)

	host := newFakeHost()
	ctx := contextFromHost(host)
	server := newServer(cfg, middlewares, routes, inputs.listeners)
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
	server, ctx, host := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

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
	assert.Equal(t, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, tasks[0].identity)
	assert.True(t, tasks[0].critical)
}

func TestAddrReturnsRealBoundEphemeralPort(t *testing.T) {
	server, ctx, _ := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()

	assert.NotEqual(t, "127.0.0.1:0", addr)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)
}

func TestStopBeforeTrafficGateReleaseDoesNotLeakListener(t *testing.T) {
	server, ctx, _ := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})

	require.NoError(t, server.Start(ctx))
	addr := server.Addr()
	require.NoError(t, server.Stop(context.Background()))

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "Stop must release a listener whose serving task is still behind the gate")
	require.NoError(t, listener.Close())
}

func TestStartFailsWhenCriticalServingTaskIsRejected(t *testing.T) {
	server, ctx, host := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{})
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
		order: Order{Phase: PhaseBusiness},
		handler: func(c *gin.Context) {
			ran = true
			c.Next()
		},
	}
	server, ctx, _ := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/api"}, serverInputs{
		middlewares: []plugin.Entry[Middleware]{
			{Identity: plugin.Identity{Plugin: "audit"}, Value: middleware},
		},
	})

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ping", nil))

	assert.Equal(t, http.StatusOK, response.Code)
	assert.True(t, ran, "middleware must be installed before the base-path Router group is created")
}

func TestOpenTrafficPropagatesRouteCatalogListenerErrorWithoutReleasingGate(t *testing.T) {
	wantErr := errors.New("swagger generation failed")
	listener := fakeRouteCatalogListener{ready: func(RouteCatalog) error { return wantErr }}
	server, ctx, host := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{
		listeners: []plugin.Entry[RouteCatalogListener]{
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
	invalidRoute := fakeRouteContributor{register: func(router *Router) {
		router.GET("/private", func(*gin.Context) {}).Auth(Accepts())
	}}
	listener := fakeRouteCatalogListener{ready: func(RouteCatalog) error {
		listenerCalled = true
		return nil
	}}
	server, ctx, host := newPingServer(t, Config{Addr: "127.0.0.1:0", BasePath: "/"}, serverInputs{
		routes: []plugin.Entry[RouteContributor]{
			{Identity: plugin.Identity{Plugin: "invalid-route"}, Value: invalidRoute},
		},
		listeners: []plugin.Entry[RouteCatalogListener]{
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
