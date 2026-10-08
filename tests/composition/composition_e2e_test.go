package composition_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/jwt"
	casbinhttp "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
	"github.com/xbcio/xbc/transport/web/extensions/authorization/tenant"
	"github.com/xbcio/xbc/transport/web/extensions/observability/auditlog"
)

// fakeHost is the smallest faithful plugin.RuntimeHost needed to drive a real
// Web server's lifecycle in a test, mirroring transport/web/fakehost_test.go
// (package-private to web_test, so this standalone composition-test module
// needs its own copy). It owns one traffic gate and one cancellable task
// scope, just like runtime.
type fakeHost struct {
	mu sync.Mutex

	execution context.Context
	logger    log.Logger
	gate      chan struct{}
	gateOnce  sync.Once

	acceptTasks bool
	taskContext context.Context
	cancelTasks context.CancelFunc
	wg          sync.WaitGroup
}

var _ plugin.RuntimeHost = (*fakeHost)(nil)

func newFakeHost() *fakeHost {
	execution, cancel := context.WithCancel(context.Background())
	return &fakeHost{
		execution:   execution,
		logger:      log.Nop(),
		gate:        make(chan struct{}),
		acceptTasks: true,
		taskContext: execution,
		cancelTasks: cancel,
	}
}

func (h *fakeHost) ExecutionContext() context.Context { return h.execution }
func (h *fakeHost) Logger() log.Logger                { return h.logger }
func (*fakeHost) ProcessInstance() string             { return "test-process" }
func (h *fakeHost) TrafficGate() <-chan struct{}      { return h.gate }

func (h *fakeHost) SubmitTask(id plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.mu.Lock()
	if !h.acceptTasks {
		h.mu.Unlock()
		return false
	}
	h.wg.Add(1)
	taskContext := h.taskContext
	h.mu.Unlock()

	go func() {
		defer h.wg.Done()
		fn(taskContext)
	}()
	return true
}

func (*fakeHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (h *fakeHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *fakeHost) shutdown() {
	h.cancelTasks()
	h.wg.Wait()
}

// recordingSink captures every auditlog event the composed server observes.
// It is the test's window into what tenant/casbin/auditlog each saw for the
// same request, independent of the HTTP response. auditlog.Config's default
// is synchronous dispatch (Async: false), so Write runs on the request
// goroutine, but the mutex keeps this safe if a future change in this test
// enables Async.
type recordingSink struct {
	mu     sync.Mutex
	events []auditlog.Event
}

func (s *recordingSink) Write(_ context.Context, event auditlog.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *recordingSink) last() auditlog.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[len(s.events)-1]
}

// probeKey identifies the test's own RouteContributor, registering routes
// that exercise every composition scenario this test asserts on.
const probeKey plugin.Key = "composition-e2e-probe"

type probePlugin struct{}

var _ web.RouteContributor = (*probePlugin)(nil)

var probeDefinition = plugin.Define(
	probeKey,
	func(plugin.BuildContext) (*probePlugin, error) { return &probePlugin{}, nil },
	plugin.Options[*probePlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *probePlugin) web.RouteContributor { return value }),
		),
	},
)

func (*probePlugin) RegisterRoutes(router *web.Router) {
	router.GET("/public", probeOK).Name("probe.public").Auth(web.Public())
	router.GET("/reports", probeOK).Name("probe.reports").Perm("reports:read")
}

func probeOK(_ context.Context, c *web.Ctx) error {
	principal, _ := web.CurrentPrincipal(c)
	body := map[string]string{"subject": principal.Subject}
	// resolvedTenant lets the test assert on what tenant actually saw for this
	// request, the same way it already reads auditlog's observed Subject from
	// the sink: tenant publishes nothing to the response on its own, so the
	// probe echoes tenant.Current back for the test to compare against
	// casbin's and auditlog's view of the same request.
	if resolved, ok := tenant.Current(c); ok {
		body["tenant"] = resolved.ID
	}
	c.JSON(http.StatusOK, body)
	return nil
}

// composeServer builds a real Web server from real plugin Definitions and
// Bundles -- jwt, apikey, tenant, casbin, auditlog, and the gin engine -- and
// drives its lifecycle (Start, traffic preparation, traffic-gate release)
// exactly as runtime's own assembly adapter does, using the fakeHost pattern
// proven in transport/web/fakehost_test.go. It returns the bound address and
// a stop function the caller must call to drain and release every resource.
func composeServer(t *testing.T, sink *recordingSink, securityPolicies []web.PolicyRule) (addr string, jwtPlugin *jwt.Plugin, stop func()) {
	t.Helper()

	jwtSecret := "composition-e2e-test-secret-32-bytes-min"
	apiKeyPlain := "composition-e2e-test-api-key-32-bytes-min"
	digest := apikey.HashKey(apiKeyPlain)

	jwtConfig := jwt.DefaultConfig()
	jwtConfig.Secret = jwtSecret
	jwtPlugin, err := jwt.New(jwtConfig)
	require.NoError(t, err)

	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"jwt": map[string]any{
				"secret": jwtSecret,
			},
			"apikey": map[string]any{
				"static": []map[string]any{{
					"id":      "probe-key",
					"subject": "alice",
					"sha256":  digest.String(),
				}},
			},
			"tenant": map[string]any{
				"required": false,
			},
			"casbin": map[string]any{
				"policy": "p, alice, reports:read\np, carol, reports:read",
			},
			"casbin-http": map[string]any{
				"missing_permission": "deny",
			},
		},
		"web": map[string]any{
			"addr": "127.0.0.1:0",
			"security": map[string]any{
				"default":  "deny",
				"schemes":  []string{"jwt", "apikey"},
				"policies": toPolicyMaps(securityPolicies),
			},
		},
	}, "")
	require.NoError(t, err)

	// auditlog.Definition() has no plugin-graph input for a custom Sink: a
	// composed application always gets NewLogSink. Reaching the real
	// *auditlog.Plugin and its real observe() middleware with an injectable
	// Sink requires auditlog.New(cfg, WithSink(...)) directly, then exporting
	// that already-built Plugin as web.Middleware through the test's own
	// Definition -- auditlog.Bundle() is deliberately not also selected, to
	// avoid two competing auditlog middlewares in the same chain.
	auditPlugin, err := auditlog.New(auditlog.DefaultConfig(), auditlog.WithSink(sink))
	require.NoError(t, err)

	bundles := []plugin.Bundle{
		web.Bundle(),
		ginengine.Bundle(),
		jwt.Bundle(),
		apikey.Bundle(),
		tenant.Bundle(),
		casbinhttp.Bundle(),
		plugin.BundleOf(probeDefinition),
		plugin.BundleOf(plugin.Define(
			"composition-e2e-auditlog",
			func(plugin.BuildContext) (*auditlog.Plugin, error) { return auditPlugin, nil },
			plugin.Options[*auditlog.Plugin]{
				Exports: plugin.Contracts(
					plugin.ExportAs[web.Middleware](func(value *auditlog.Plugin) web.Middleware { return value }),
				),
			},
		)),
	}

	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: bundles,
		Env:     environment,
	})
	require.NoError(t, err)

	host := newFakeHost()
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	require.NoError(t, err)

	instance, found := constructed.Instance(plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance})
	require.True(t, found, "web instance not constructed")
	server, ok := instance.Primary().(*web.Server)
	require.True(t, ok, "primary = %T, want *web.Server", instance.Primary())

	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())
	host.releaseTraffic()

	require.Eventually(t, func() bool { return server.Addr() != "" }, 2*time.Second, 5*time.Millisecond)

	stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// StopBounded invokes Server.Stop, which calls engine.Shutdown to close
		// the listener and unblock the serving goroutine's Accept loop. That
		// must happen before waiting on the host's task WaitGroup: task
		// cancellation alone never unblocks a goroutine parked in Accept.
		_ = instance.StopBounded(ctx, 5*time.Second)
		host.shutdown()
	}
	return server.Addr(), jwtPlugin, stop
}

func toPolicyMaps(rules []web.PolicyRule) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		m := map[string]any{"match": rule.Match}
		if rule.Permit {
			m["permit"] = true
		}
		if len(rule.Authenticate) > 0 {
			schemes := make([]string, len(rule.Authenticate))
			for i, s := range rule.Authenticate {
				schemes[i] = string(s)
			}
			m["authenticate"] = schemes
		}
		out = append(out, m)
	}
	return out
}

func get(t *testing.T, addr, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s%s", addr, path), nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// probeBody decodes the probe handler's JSON response, giving the test a
// window into what the request's own handler observed -- principal subject
// and resolved tenant -- independent of auditlog's sink.
func probeBody(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// TestCompositionE2EPublicRestrictedAndSchemeArbitration builds a real Web
// server composed from real jwt, apikey, tenant, casbin, and auditlog plugin
// Definitions/Bundles and drives it with real HTTP requests over an
// ephemeral loopback listener. It asserts the end-to-end contract Batch A's
// Principal/RequiresPrincipal model exists to guarantee:
//   - a public route is reachable with no credential at all;
//   - a restrictive route (web.security default: deny, no covering policy)
//     rejects an anonymous request;
//   - either declared scheme (jwt or apikey) is independently sufficient;
//   - when both credentials are present, the declared Schemes order (jwt
//     before apikey) decides which one wins arbitration;
//   - tenant, casbin, and auditlog all observe the one Subject the manager
//     resolved for a request, never a mismatched or partial one;
//   - a malformed credential never yields access, under either scheme.
//
// session is deliberately not composed here. Unlike jwt and apikey, which
// authenticate from a self-contained request header, session authenticates
// from server-side state a test would have to create out of band -- calling
// session.Manager.Create and attaching the resulting cookie before the
// composed request -- which is a second, materially different setup path
// rather than one more scheme alongside jwt/apikey in composeServer. That
// setup is exercised in session's own package tests; this suite's purpose is
// the cross-plugin Principal/tenant/casbin/auditlog agreement, which a
// session-authenticated Principal would exercise identically to jwt's.
func TestCompositionE2EPublicRestrictedAndSchemeArbitration(t *testing.T) {
	sink := &recordingSink{}
	addr, jwtPlugin, stop := composeServer(t, sink, nil)
	defer stop()

	t.Run("public route is reachable anonymously", func(t *testing.T) {
		resp := get(t, addr, "/public", nil)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("restrictive default rejects anonymous", func(t *testing.T) {
		resp := get(t, addr, "/reports", nil)
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	aliceToken, err := jwtPlugin.Sign("alice", jwt.Claims{"tenant_id": "acme"})
	require.NoError(t, err)

	t.Run("jwt alone is accepted", func(t *testing.T) {
		resp := get(t, addr, "/reports", map[string]string{
			"Authorization": "Bearer " + aliceToken,
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "acme", probeBody(t, resp)["tenant"],
			"tenant must resolve the tenant_id claim the Manager's selected jwt Principal carried")
	})

	t.Run("apikey alone is accepted", func(t *testing.T) {
		resp := get(t, addr, "/reports", map[string]string{
			"X-API-Key": "composition-e2e-test-api-key-32-bytes-min",
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Empty(t, probeBody(t, resp)["tenant"],
			"apikey's static Principal carries no tenant_id attribute, so tenant.required: false must leave it unresolved rather than inventing one")
	})

	t.Run("declared scheme order arbitrates when both credentials present", func(t *testing.T) {
		// carol only has a casbin policy for apikey's subject; alice's JWT names
		// a different subject. Presenting both an apikey (subject=alice, valid)
		// and a JWT for a subject with NO casbin policy proves which credential
		// the Manager actually picked: Schemes is ["jwt", "apikey"], so the JWT
		// (subject carol, no policy) must win and the request must be forbidden
		// by casbin -- not silently fall back to the valid apikey.
		carolToken, err := jwtPlugin.Sign("carol-no-policy", nil)
		require.NoError(t, err)

		before := sink.count()
		resp := get(t, addr, "/reports", map[string]string{
			"Authorization": "Bearer " + carolToken,
			"X-API-Key":     "composition-e2e-test-api-key-32-bytes-min",
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "jwt (declared first) must win arbitration over apikey")
		require.Greater(t, sink.count(), before)
		event := sink.last()
		require.Equal(t, "carol-no-policy", event.Subject, "auditlog must observe the jwt subject the Manager actually selected")
		require.Equal(t, "jwt", event.AuthMethod)
	})

	t.Run("tenant casbin and auditlog observe the same subject", func(t *testing.T) {
		before := sink.count()
		resp := get(t, addr, "/reports", map[string]string{
			"Authorization": "Bearer " + aliceToken,
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := probeBody(t, resp)
		require.Equal(t, "acme", body["tenant"],
			"tenant must resolve the same Principal casbin went on to authorize")
		require.Greater(t, sink.count(), before)
		event := sink.last()
		require.Equal(t, "alice", event.Subject, "auditlog's observed Subject must match the Principal casbin authorized against")
		require.Equal(t, "jwt", event.AuthMethod)
	})

	t.Run("malformed jwt never yields access", func(t *testing.T) {
		resp := get(t, addr, "/reports", map[string]string{
			"Authorization": "Bearer not-a-real-token",
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("malformed apikey never yields access", func(t *testing.T) {
		resp := get(t, addr, "/reports", map[string]string{
			"X-API-Key": "wrong-key-wrong-key-wrong-key-32b",
		})
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// TestTenantOrdersBeforeTheCasbinMiddlewareItNames is the cross-module pin for
// a soft ordering edge neither module can prove alone. tenant is a
// transport/web built-in and must not import the extension module that owns
// the casbin-http middleware, so it duplicates the plugin key as a typed
// constant, and web.Prefer degrades silently: if the two strings drift,
// ordering falls back to lexicographic tie-break, which sorts tenant after
// casbin-http -- tenant resolution would run after route authorization instead
// of before it -- with no compile error and no failing test in either module.
// This module imports both, so it is the only place the identities can be
// compared.
func TestTenantOrdersBeforeTheCasbinMiddlewareItNames(t *testing.T) {
	before := (&tenant.Plugin{}).Order().Before
	require.Len(t, before, 1, "tenant must declare exactly one ordering preference")
	require.Equal(t, casbinhttp.Key, before[0].Key(),
		"tenant's optional ordering target must be the key the casbin-http middleware actually registers")
	require.Empty(t, before[0].InstanceName(), "the preference targets the default instance, not a named one")
	require.False(t, before[0].Required(),
		"the preference must stay soft: a custom authorization stack without casbin-http remains valid")
}
