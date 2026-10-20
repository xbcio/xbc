package idempotency

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// This file drives the idempotency middleware through a real Web server
// assembled from real plugin Definitions: the frozen route table, the
// recordCurrentRoute wiring Router.Handle bakes into every registered route,
// the built-in authentication middleware and the real middleware ordering
// graph. The unit tests around it build request chains by hand and inject
// RouteInfo directly; this test is the end-to-end check that the same
// guarantees hold when the route is registered through Router.POST,
// Router.Freeze produces the table, and requests arrive over a listener.

// routerIntegrationHost is the smallest RuntimeHost that can drive a real Web
// server through Start and traffic preparation, mirroring the fake host the
// composition tests use.
type routerIntegrationHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*routerIntegrationHost)(nil)

func newRouterIntegrationHost() *routerIntegrationHost {
	execution, cancel := context.WithCancel(context.Background())
	return &routerIntegrationHost{execution: execution, cancel: cancel, gate: make(chan struct{})}
}

func (h *routerIntegrationHost) ExecutionContext() context.Context { return h.execution }
func (*routerIntegrationHost) Logger() log.Logger                  { return log.Nop() }
func (*routerIntegrationHost) ProcessInstance() string             { return "idempotency-router-test" }
func (*routerIntegrationHost) Service() plugin.ServiceIdentity     { return plugin.ServiceIdentity{} }
func (*routerIntegrationHost) IngressStopBudget() time.Duration    { return 0 }
func (h *routerIntegrationHost) TrafficGate() <-chan struct{}      { return h.gate }

func (*routerIntegrationHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*routerIntegrationHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission {
	return nil
}

func (h *routerIntegrationHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
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

func (*routerIntegrationHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (h *routerIntegrationHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *routerIntegrationHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

// routerIntegrationPrincipal publishes a fixed principal in PhaseAuth, the
// same position a real authentication plugin's middleware occupies. The probe
// route is declared public, so the built-in authentication middleware permits
// it and leaves the published principal in place; idempotency then sees the
// subject it scopes a stored response to.
type routerIntegrationPrincipal struct{}

var _ web.Middleware = (*routerIntegrationPrincipal)(nil)

func (*routerIntegrationPrincipal) Handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "test"})
		c.Next()
		return nil
	}
}

func (*routerIntegrationPrincipal) Order() web.Order { return web.Order{Phase: web.PhaseAuth} }

// routerIntegrationRoutes registers the templated cancellation route through
// the real Router, which is what makes CurrentRoute publish a route template
// while the concrete request path differs from it.
type routerIntegrationRoutes struct {
	calls *atomic.Int32
}

var _ web.RouteContributor = (*routerIntegrationRoutes)(nil)

func (r *routerIntegrationRoutes) RegisterRoutes(router *web.Router) {
	router.POST("/orders/{id}/cancel", func(_ context.Context, c *web.Ctx) error {
		call := r.calls.Add(1)
		path := c.Request().URL.Path
		c.SetHeader("Location", "/receipts"+path)
		c.SetHeader("ETag", fmt.Sprintf(`"call-%d"`, call))
		c.SetHeader("Cache-Control", "no-store")
		c.String(http.StatusOK, "call-%d", call)
		return nil
	}).Name("orders.cancel").Idempotent().Auth(web.Public())
}

var (
	routerIntegrationEngineDefinition = plugin.Define(
		"idempotency-router-engine",
		func(plugin.BuildContext) (enginetest.Factory, error) { return enginetest.Factory{}, nil },
		plugin.Options[enginetest.Factory]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.EngineFactory](func(value enginetest.Factory) web.EngineFactory { return value }),
			),
		},
	)

	routerIntegrationPrincipalDefinition = plugin.Define(
		"idempotency-router-principal",
		func(plugin.BuildContext) (*routerIntegrationPrincipal, error) {
			return &routerIntegrationPrincipal{}, nil
		},
		plugin.Options[*routerIntegrationPrincipal]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.Middleware](func(value *routerIntegrationPrincipal) web.Middleware { return value }),
			),
		},
	)
)

func routerIntegrationRoutesDefinition(calls *atomic.Int32) plugin.Definition {
	return plugin.Define(
		"idempotency-router-routes",
		func(plugin.BuildContext) (*routerIntegrationRoutes, error) {
			return &routerIntegrationRoutes{calls: calls}, nil
		},
		plugin.Options[*routerIntegrationRoutes]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *routerIntegrationRoutes) web.RouteContributor { return value }),
			),
		},
	)
}

type routerIntegrationResponse struct {
	code     int
	body     string
	headers  http.Header
	replayed string
}

func routerIntegrationRequest(t *testing.T, url, key string) routerIntegrationResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Idempotency-Key", key)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return routerIntegrationResponse{
		code:     response.StatusCode,
		body:     string(body),
		headers:  response.Header,
		replayed: response.Header.Get("Idempotency-Replayed"),
	}
}

// TestRealRouterScopesIdempotencyToTheConcreteRequestPath is the end-to-end
// counterpart of TestConcreteRequestPathsAreIndependentIdempotencyScopes: the
// same key on two path-parameter values routed through one frozen template
// executes both handlers and replays only a repeat of the same concrete path,
// with the whitelisted response headers restored from the real route.
func TestRealRouterScopesIdempotencyToTheConcreteRequestPath(t *testing.T) {
	calls := new(atomic.Int32)
	environment, err := config.NewEnvironment(map[string]any{
		"web": map[string]any{"addr": "127.0.0.1:0"},
		"plugins": map[string]any{
			"idempotency": map[string]any{},
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	host := newRouterIntegrationHost()
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{
			web.Bundle(),
			Bundle(),
			plugin.BundleOf(routerIntegrationEngineDefinition),
			plugin.BundleOf(routerIntegrationPrincipalDefinition),
			plugin.BundleOf(routerIntegrationRoutesDefinition(calls)),
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
	instance, found := constructed.Instance(plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("web instance not constructed")
	}
	server, ok := instance.Primary().(*web.Server)
	if !ok {
		t.Fatalf("primary = %T, want *web.Server", instance.Primary())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = instance.StopBounded(ctx, 5*time.Second)
		host.shutdown()
	})
	if err := instance.InvokeStart(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := instance.InvokeTrafficPreparation(); err != nil {
		t.Fatalf("traffic preparation error = %v", err)
	}
	host.releaseTraffic()
	deadline := time.Now().Add(5 * time.Second)
	for server.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.Addr() == "" {
		t.Fatal("server did not bind an address")
	}
	base := "http://" + server.Addr()

	first := routerIntegrationRequest(t, base+"/orders/1/cancel", "router-integration-key")
	second := routerIntegrationRequest(t, base+"/orders/2/cancel", "router-integration-key")
	repeat := routerIntegrationRequest(t, base+"/orders/1/cancel", "router-integration-key")
	if first.code != http.StatusOK || first.body != "call-1" || second.code != http.StatusOK || second.body != "call-2" {
		t.Fatalf("first=%d %q second=%d %q, want each concrete path to execute", first.code, first.body, second.code, second.body)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if second.replayed != "" {
		t.Fatalf("second concrete path replayed the first response: %q", second.replayed)
	}
	if repeat.code != http.StatusOK || repeat.body != "call-1" || repeat.replayed != "true" {
		t.Fatalf("repeat=%d %q replay=%q, want the first concrete path replayed", repeat.code, repeat.body, repeat.replayed)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after replay, want 2", calls.Load())
	}
	if got := repeat.headers.Get("Location"); got != "/receipts/orders/1/cancel" {
		t.Fatalf("replayed Location = %q", got)
	}
	if got := repeat.headers.Get("ETag"); got != `"call-1"` {
		t.Fatalf("replayed ETag = %q", got)
	}
	if got := repeat.headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("replayed Cache-Control = %q", got)
	}
}
