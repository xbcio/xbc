package websocket

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// testScheme is the single authentication scheme the test composition
// registers. A protected route needs a real authenticator behind it, otherwise
// the Web server refuses to start with the deny default.
const testScheme authentication.Scheme = "test"

// testHost is the smallest plugin.RuntimeHost that can drive a real Web server
// through Start and traffic preparation, mirroring the fake host the
// composition tests use.
type testHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	execution, cancel := context.WithCancel(context.Background())
	return &testHost{execution: execution, cancel: cancel, gate: make(chan struct{})}
}

func (h *testHost) ExecutionContext() context.Context { return h.execution }
func (*testHost) Logger() log.Logger                  { return log.Nop() }
func (*testHost) ProcessInstance() string             { return "websocket-test" }
func (*testHost) Service() plugin.ServiceIdentity     { return plugin.ServiceIdentity{} }
func (*testHost) IngressStopBudget() time.Duration    { return 0 }
func (h *testHost) TrafficGate() <-chan struct{}      { return h.gate }

func (*testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission {
	return nil
}

func (h *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.execution)
	}()
	return true
}

func (*testHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (h *testHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *testHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

// testAuthenticator accepts any presented credential and publishes it as the
// principal subject, so the composition has exactly one reachable scheme.
type testAuthenticator struct{}

func (*testAuthenticator) Scheme() authentication.Scheme { return testScheme }

func (*testAuthenticator) Authenticate(_ context.Context, credential authentication.Credential) (authentication.Result, error) {
	value, _ := authentication.CredentialAs[string](credential)
	if value == "" {
		return authentication.Rejected(authentication.ReasonInvalidCredential), nil
	}
	return authentication.Accepted(authentication.Principal{Subject: value, AuthMethod: string(testScheme)}), nil
}

// testExtractor reads the test credential from one request header.
type testExtractor struct{}

func (*testExtractor) Scheme() authentication.Scheme { return testScheme }

func (*testExtractor) ExtractCredential(c *web.Ctx) (authentication.CredentialResult, error) {
	value := c.GetHeader("X-Test-Credential")
	if value == "" {
		return authentication.Absent(), nil
	}
	return authentication.Presented(value), nil
}

// recordedRequest is one request the recording middleware observed.
type recordedRequest struct {
	path    string
	upgrade string
}

// requestRecorder keeps the requests the middleware saw, in arrival order.
type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) record(request *http.Request) {
	if request == nil || request.URL == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{path: request.URL.Path, upgrade: request.Header.Get("Upgrade")})
}

func (r *requestRecorder) saw(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.requests {
		if request.path == path {
			return true
		}
	}
	return false
}

// recordingMiddleware observes every routed request before the handler runs.
// It records on entry, not after c.Next, because the WebSocket handler blocks
// for the life of the connection: recording after Next would make the
// observation indistinguishable from the handler returning.
type recordingMiddleware struct {
	recorder *requestRecorder
}

func (m *recordingMiddleware) Handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		m.recorder.record(c.Request())
		c.Next()
		return nil
	}
}

func (*recordingMiddleware) Order() web.Order { return web.Order{Phase: web.PhaseBusiness} }

var upgraderInput = plugin.RequireOne[Upgrader]()

var testEngineDefinition = plugin.Define(
	"websocket-test-engine",
	func(plugin.BuildContext) (enginetest.Factory, error) { return enginetest.Factory{}, nil },
	plugin.Options[enginetest.Factory]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.EngineFactory](func(value enginetest.Factory) web.EngineFactory { return value }),
		),
	},
)

var testAuthenticatorDefinition = plugin.Define(
	"websocket-test-authenticator",
	func(plugin.BuildContext) (*testAuthenticator, error) { return &testAuthenticator{}, nil },
	plugin.Options[*testAuthenticator]{
		Exports: plugin.Contracts(
			plugin.ExportAs[authentication.Authenticator](func(value *testAuthenticator) authentication.Authenticator { return value }),
		),
	},
)

var testExtractorDefinition = plugin.Define(
	"websocket-test-extractor",
	func(plugin.BuildContext) (*testExtractor, error) { return &testExtractor{}, nil },
	plugin.Options[*testExtractor]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.CredentialExtractor](func(value *testExtractor) web.CredentialExtractor { return value }),
		),
	},
)

func testRecorderDefinition(recorder *requestRecorder) plugin.Definition {
	return plugin.Define(
		"websocket-test-recorder",
		func(plugin.BuildContext) (*recordingMiddleware, error) {
			return &recordingMiddleware{recorder: recorder}, nil
		},
		plugin.Options[*recordingMiddleware]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.Middleware](func(value *recordingMiddleware) web.Middleware { return value }),
			),
		},
	)
}

// testRoutes records what a handler test needs: the two routes share one
// Upgrader resolved from the graph, one echo endpoint declared public and one
// endpoint left under the deny-by-default authentication policy.
type testRoutes struct {
	upgrader Upgrader
	options  Options
	serve    func(context.Context, *Conn) error
}

func testRoutesDefinition(options Options, serve func(context.Context, *Conn) error) plugin.Definition {
	return plugin.Define(
		"websocket-test-routes",
		func(ctx plugin.BuildContext) (*testRoutes, error) {
			return &testRoutes{upgrader: upgraderInput.Get(ctx).Value, options: options, serve: serve}, nil
		},
		plugin.Options[*testRoutes]{
			Inputs: plugin.Inputs(upgraderInput),
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *testRoutes) web.RouteContributor { return value }),
			),
		},
	)
}

func (r *testRoutes) RegisterRoutes(router *web.Router) {
	router.GET("/ws", r.upgrader.Handler(r.options, r.serve)).Name("ws.public").Auth(web.Public())
	router.GET("/ws-protected", r.upgrader.Handler(r.options, r.serve)).Name("ws.protected")
}

// testApp is one running composition: the Web server, the WebSocket plugin the
// graph constructed, the recording middleware, and a client whose transport is
// closed with the test so no idle connection outlives it.
type testApp struct {
	server   *web.Server
	plugin   *Plugin
	host     *testHost
	client   *http.Client
	baseURL  string
	wsURL    string
	recorder *requestRecorder
}

// startTestApp composes web.Bundle with the WebSocket Bundle, the neutral test
// engine, the test authenticator and extractor, a recording middleware, and
// one route contributor that registers the two test endpoints.
func startTestApp(t *testing.T, options Options, serve func(context.Context, *Conn) error, maxInFlight int) *testApp {
	t.Helper()
	recorder := &requestRecorder{}
	webConfig := map[string]any{"addr": "127.0.0.1:0"}
	if maxInFlight > 0 {
		webConfig["max_in_flight"] = maxInFlight
	}
	environment, err := config.NewEnvironment(map[string]any{"web": webConfig}, "")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	host := newTestHost()
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{
			web.Bundle(),
			Bundle(),
			plugin.BundleOf(testEngineDefinition),
			plugin.BundleOf(testAuthenticatorDefinition),
			plugin.BundleOf(testExtractorDefinition),
			plugin.BundleOf(testRecorderDefinition(recorder)),
			plugin.BundleOf(testRoutesDefinition(options, serve)),
		},
		Env: environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	webInstance, found := constructed.Instance(plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("web instance not constructed")
	}
	server, ok := webInstance.Primary().(*web.Server)
	if !ok {
		t.Fatalf("web primary = %T, want *web.Server", webInstance.Primary())
	}
	websocketInstance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("websocket instance not constructed")
	}
	value, ok := websocketInstance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("websocket primary = %T, want *Plugin", websocketInstance.Primary())
	}
	transport := &http.Transport{}
	app := &testApp{
		server:   server,
		plugin:   value,
		host:     host,
		client:   &http.Client{Transport: transport},
		recorder: recorder,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// The plugin closes connections the HTTP server can no longer see;
		// this must run before the server's own shutdown. A test that already
		// stopped the plugin hits the idempotent path.
		_ = app.plugin.Stop(ctx)
		_ = webInstance.StopBounded(ctx, 5*time.Second)
		host.shutdown()
		transport.CloseIdleConnections()
	})
	if err := webInstance.InvokeStart(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := webInstance.InvokeTrafficPreparation(); err != nil {
		t.Fatalf("traffic preparation error = %v", err)
	}
	host.releaseTraffic()
	waitFor(t, 10*time.Second, "server to bind an address", func() bool { return server.Addr() != "" })
	app.baseURL = "http://" + server.Addr()
	app.wsURL = "ws://" + server.Addr()
	return app
}

// dial opens one WebSocket connection to path, adapting the caller's options
// to the test client so keep-alive state stays under the test's control.
func (a *testApp) dial(t *testing.T, path string, options coderwebsocket.DialOptions) (*Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	options.HTTPClient = a.client
	return coderwebsocket.Dial(ctx, a.wsURL+path, &options)
}

// get issues one plain HTTP request through the same client.
func (a *testApp) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	return a.getWithHeaders(t, path, nil)
}

// getWithHeaders issues one plain HTTP request with extra request headers.
func (a *testApp) getWithHeaders(t *testing.T, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := a.client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	return response, readAll(t, response)
}

// readAll reads a response body that the protocol library may have bounded.
func readAll(t *testing.T, response *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}

// dialContext dials without registering test cleanup, so it is safe to call
// from the concurrency test's goroutines.
func (a *testApp) dialContext(ctx context.Context, path string, options coderwebsocket.DialOptions) (*Conn, *http.Response, error) {
	options.HTTPClient = a.client
	return coderwebsocket.Dial(ctx, a.wsURL+path, &options)
}

// registryEmpty reports whether the plugin's registry holds no connection. It
// reads the same state the lifecycle does, under the same lock.
func (p *Plugin) registryEmpty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns) == 0
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
