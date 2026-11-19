package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// testHost is the smallest plugin.RuntimeHost that can drive a real Web server
// through Start and traffic preparation.
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

func (host *testHost) ExecutionContext() context.Context { return host.execution }
func (*testHost) Logger() log.Logger                     { return log.Nop() }
func (*testHost) ProcessInstance() string                { return "session-http-test" }
func (*testHost) Service() plugin.ServiceIdentity        { return plugin.ServiceIdentity{} }
func (*testHost) IngressStopBudget() time.Duration       { return 0 }
func (host *testHost) TrafficGate() <-chan struct{}      { return host.gate }

func (*testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission {
	return nil
}

func (host *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	host.tasks.Add(1)
	go func() {
		defer host.tasks.Done()
		fn(host.execution)
	}()
	return true
}

func (*testHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (host *testHost) releaseTraffic() { host.gateOnce.Do(func() { close(host.gate) }) }

func (host *testHost) shutdown() {
	host.cancel()
	host.tasks.Wait()
}

var testEngineDefinition = plugin.Define(
	"session-http-test-engine",
	func(plugin.BuildContext) (enginetest.Factory, error) { return enginetest.Factory{}, nil },
	plugin.Options[enginetest.Factory]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.EngineFactory](func(value enginetest.Factory) web.EngineFactory { return value }),
		),
	},
)

var testSessionInput = plugin.RefTo[*Plugin](Key)

type testRoutes struct {
	sessions *Plugin
}

func testRoutesDefinition() plugin.Definition {
	return plugin.Define(
		"session-http-test-routes",
		func(ctx plugin.BuildContext) (*testRoutes, error) {
			return &testRoutes{sessions: testSessionInput.Get(ctx).Value}, nil
		},
		plugin.Options[*testRoutes]{
			Inputs: plugin.Inputs(testSessionInput),
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *testRoutes) web.RouteContributor { return value }),
			),
		},
	)
}

func (routes *testRoutes) RegisterRoutes(router *web.Router) {
	router.POST("/login", routes.login).Name("login").Auth(web.Public())
	router.POST("/logout", routes.logout).Name("logout").Auth(web.Public())
	router.GET("/protected", routes.protected).Name("protected").Auth(web.Accepts(sessioncore.Scheme))
}

func (routes *testRoutes) login(ctx context.Context, c *web.Ctx) error {
	if _, err := routes.sessions.Login(ctx, c, "alice", map[string]any{"role": "admin"}); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

func (routes *testRoutes) logout(ctx context.Context, c *web.Ctx) error {
	if err := routes.sessions.Logout(ctx, c); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

func (*testRoutes) protected(_ context.Context, c *web.Ctx) error {
	view, ok := Current(c)
	if !ok {
		return errors.New("session-http-test: protected route has no principal")
	}
	c.JSON(http.StatusOK, map[string]any{"subject": view.Subject})
	return nil
}

type testApp struct {
	server  *web.Server
	plugin  *Plugin
	host    *testHost
	client  *http.Client
	baseURL string
}

// startTestApp composes the Web server, the real session bundle, the neutral
// test engine, and the test routes. pluginsConfig becomes the plugins section;
// an empty map still needs plugins.session to activate the pair.
func startTestApp(t *testing.T, pluginsConfig map[string]any) *testApp {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{
		"web":     map[string]any{"addr": "127.0.0.1:0"},
		"plugins": pluginsConfig,
	}, "")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{
			web.Bundle(),
			Bundle(),
			plugin.BundleOf(testEngineDefinition),
			plugin.BundleOf(testRoutesDefinition()),
		},
		Env: environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan() error = %v", err)
	}
	host := newTestHost()
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct() error = %v", err)
	}
	webInstance, found := constructed.Instance(plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("Web server instance not constructed")
	}
	server, ok := webInstance.Primary().(*web.Server)
	if !ok {
		t.Fatalf("Web server primary = %T", webInstance.Primary())
	}
	sessionInstance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("session adapter instance not constructed")
	}
	adapter, ok := sessionInstance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("session adapter primary = %T", sessionInstance.Primary())
	}
	transport := &http.Transport{}
	app := &testApp{
		server: server,
		plugin: adapter,
		host:   host,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = sessionInstance.StopBounded(ctx, 5*time.Second)
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
	return app
}

func (app *testApp) do(t *testing.T, method, path string, cookie *http.Cookie, headers map[string]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, app.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := app.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	return response, string(body)
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

func loginCookie(t *testing.T, app *testApp) *http.Cookie {
	t.Helper()
	response, _ := app.do(t, http.MethodPost, "/login", nil, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /login = %d, want 204", response.StatusCode)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %#v", cookies)
	}
	return cookies[0]
}

// TestRevokedSubjectIsRejectedByTheAuthenticationMiddleware drives the real
// middleware: a logged-in cookie reaches a protected route, RevokeSubject ends
// the session, and the same cookie is then refused.
func TestRevokedSubjectIsRejectedByTheAuthenticationMiddleware(t *testing.T) {
	app := startTestApp(t, map[string]any{
		"session": map[string]any{"name": "sid", "secure": false},
	})
	cookie := loginCookie(t, app)

	allowed, body := app.do(t, http.MethodGet, "/protected", cookie, nil)
	if allowed.StatusCode != http.StatusOK || !strings.Contains(body, "alice") {
		t.Fatalf("protected with session = %d %q, want 200 and alice", allowed.StatusCode, body)
	}

	// The default chain is cookie-only: the header source must not authenticate.
	headerStatus, _ := app.do(t, http.MethodGet, "/protected", nil, map[string]string{
		"Authorization": "Session " + cookie.Value,
	})
	if headerStatus.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected with header only = %d, want 401 under the default chain", headerStatus.StatusCode)
	}

	removed, err := app.plugin.RevokeBySubject(context.Background(), "alice")
	if err != nil || removed != 1 {
		t.Fatalf("RevokeBySubject() = %d, %v; want 1, nil", removed, err)
	}
	rejected, rejectedBody := app.do(t, http.MethodGet, "/protected", cookie, nil)
	if rejected.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected after revoke = %d, want 401", rejected.StatusCode)
	}
	if strings.Contains(rejectedBody, cookie.Value) {
		t.Fatalf("rejection body leaked the session ID: %q", rejectedBody)
	}
}

// TestCarrierSectionWiresTheHeaderChainIntoTheMiddleware proves the optional
// configuration reaches the real extractor: the same session ID authenticates
// from the header, and logout through the header revokes it.
func TestCarrierSectionWiresTheHeaderChainIntoTheMiddleware(t *testing.T) {
	app := startTestApp(t, map[string]any{
		"session": map[string]any{"name": "sid", "secure": false},
		"session-http": map[string]any{
			"carriers": []any{"cookie", "header"},
		},
	})
	cookie := loginCookie(t, app)

	allowed, body := app.do(t, http.MethodGet, "/protected", nil, map[string]string{
		"Authorization": "Session " + cookie.Value,
	})
	if allowed.StatusCode != http.StatusOK || !strings.Contains(body, "alice") {
		t.Fatalf("protected with header = %d %q, want 200 and alice", allowed.StatusCode, body)
	}

	logout, _ := app.do(t, http.MethodPost, "/logout", nil, map[string]string{
		"Authorization": "Session " + cookie.Value,
	})
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /logout with header = %d, want 204", logout.StatusCode)
	}
	rejected, _ := app.do(t, http.MethodGet, "/protected", nil, map[string]string{
		"Authorization": "Session " + cookie.Value,
	})
	if rejected.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected after header logout = %d, want 401", rejected.StatusCode)
	}
}
