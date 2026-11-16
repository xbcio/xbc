package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestRouteCatalogListenerReceivesTheCompiledPolicy pins the exported
// read-only policy surface: every listener gets a resolver beside the frozen
// catalog, and resolving a catalog route answers with the runtime decision --
// outcome, schemes, deciding tier, and (for a rule) its index and original
// match expression. It also pins that a permit rule's reach reaches the startup
// report.
func TestRouteCatalogListenerReceivesTheCompiledPolicy(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Management.Addr = "127.0.0.1:0"
	cfg.Security.Default = web.SecurityDeny
	cfg.Security.Policies = []web.PolicyRule{
		{Match: "GET /health", Permit: true},
		{Match: "GET /api/orders", Permit: true},
		{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt"}},
	}

	public := web.Public()
	jwt := web.Accepts("jwt")
	ok := func(context.Context, *web.Ctx) error { return nil }
	contributor := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/health", ok).Name("policy.health")
		router.GET("/api/orders", ok).Name("policy.orders")
		router.GET("/api/other", ok).Name("policy.other")
		router.GET("/declared-public", ok).Auth(public).Name("policy.declaredPublic")
		router.GET("/declared-jwt", ok).Auth(jwt).Name("policy.declaredJWT")
		router.GET("/uncovered", ok).Name("policy.uncovered")
		router.Management().GET("/-/metrics", ok).Name("policy.metrics")
	}}

	var catalog web.RouteCatalog
	var resolver web.PolicyResolver
	listener := fakeRouteCatalogListener{ready: func(received web.RouteCatalog, policies web.PolicyResolver) error {
		catalog = received
		resolver = policies
		return nil
	}}

	host := newFakeHost()
	logger := &captureLogger{Logger: log.Nop()}
	host.logger = logger
	ctx := contextFromHost(host)
	server := web.NewServer(
		cfg,
		enginetest.Factory{},
		nil,
		nil,
		[]plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "policytest"}, Value: contributor},
		},
		[]plugin.Entry[web.RouteCatalogListener]{
			{Identity: plugin.Identity{Plugin: "policylistener"}, Value: listener},
		},
		[]plugin.Entry[authentication.Authenticator]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubAuth{scheme: "jwt"}},
		},
		[]plugin.Entry[web.CredentialExtractor]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubExtractor{scheme: "jwt"}},
		},
	)
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	require.NoError(t, server.Preflight(ctx))
	require.NotNil(t, catalog, "the listener must receive the frozen catalog")
	require.Empty(t, server.Addr(), "Preflight must not bind the configured address")

	tests := []struct {
		name          string
		method        string
		path          string
		wantOutcome   web.PolicyOutcome
		wantSchemes   []authentication.Scheme
		wantSource    web.PolicySource
		wantRuleIndex int
		wantRuleMatch string
	}{
		{
			name:          "application rule permits",
			method:        http.MethodGet,
			path:          "/health",
			wantOutcome:   web.PolicyPermit,
			wantSource:    web.PolicySourceRule,
			wantRuleIndex: 0,
			wantRuleMatch: "GET /health",
		},
		{
			name:          "first matching rule wins",
			method:        http.MethodGet,
			path:          "/api/orders",
			wantOutcome:   web.PolicyPermit,
			wantSource:    web.PolicySourceRule,
			wantRuleIndex: 1,
			wantRuleMatch: "GET /api/orders",
		},
		{
			name:          "application rule authenticates",
			method:        http.MethodGet,
			path:          "/api/other",
			wantOutcome:   web.PolicyAuthenticate,
			wantSchemes:   []authentication.Scheme{"jwt"},
			wantSource:    web.PolicySourceRule,
			wantRuleIndex: 2,
			wantRuleMatch: "/api/**",
		},
		{
			name:          "route declaration permits",
			method:        http.MethodGet,
			path:          "/declared-public",
			wantOutcome:   web.PolicyPermit,
			wantSource:    web.PolicySourceRoute,
			wantRuleIndex: -1,
		},
		{
			name:          "route declaration selects schemes",
			method:        http.MethodGet,
			path:          "/declared-jwt",
			wantOutcome:   web.PolicyAuthenticate,
			wantSchemes:   []authentication.Scheme{"jwt"},
			wantSource:    web.PolicySourceRoute,
			wantRuleIndex: -1,
		},
		{
			name:          "uncovered route falls to the deny default",
			method:        http.MethodGet,
			path:          "/uncovered",
			wantOutcome:   web.PolicyDeny,
			wantSchemes:   []authentication.Scheme{"jwt"},
			wantSource:    web.PolicySourceDefault,
			wantRuleIndex: -1,
		},
		{
			name:          "management route is not governed",
			method:        http.MethodGet,
			path:          "/-/metrics",
			wantOutcome:   web.PolicyNotApplicable,
			wantRuleIndex: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, ok := catalog.Lookup(tt.method, tt.path)
			require.True(t, ok, "catalog must contain %s %s", tt.method, tt.path)
			got := resolver.Resolve(route)
			assert.Equal(t, tt.wantOutcome, got.Outcome)
			assert.Equal(t, tt.wantSchemes, got.Schemes)
			assert.Equal(t, tt.wantRuleIndex, got.RuleIndex)
			assert.Equal(t, tt.wantRuleMatch, got.RuleMatch)
			if tt.wantOutcome != web.PolicyNotApplicable {
				assert.Equal(t, tt.wantSource, got.Source)
			}
		})
	}

	report := strings.Join(logger.infos(), "\n")
	assert.Contains(t, report, `0. "GET /health" (permit) matched 1 route`,
		"the startup report must state how many routes each permit rule matches")
	assert.Contains(t, report, `2. "/api/**" (authenticate jwt) matched 2 routes`)
}

// newPolicyBootServer assembles a minimal boot composition with one serving
// route. It exists so the mutation below changes only the policy rule under
// test.
func newPolicyBootServer(t *testing.T, cfg web.Config) (*web.Server, *plugin.Context, *fakeHost) {
	t.Helper()

	contributor := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/health", func(context.Context, *web.Ctx) error { return nil }).Name("policy.health")
	}}
	host := newFakeHost()
	ctx := contextFromHost(host)
	server := web.NewServer(
		cfg,
		enginetest.Factory{},
		nil,
		nil,
		[]plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "policytest"}, Value: contributor},
		},
		nil, nil, nil,
	)
	t.Cleanup(func() {
		_ = server.Stop(context.Background())
		host.shutdown()
	})
	return server, ctx, host
}

// TestPermitRuleMovedToUnknownPathFailsBoot is the mutation-style reverse case:
// the same permit rule boots when it names a registered route, and the boot
// must fail once that route name is changed to one the frozen table does not
// contain. The failure names the rule's index and original match text, and the
// traffic gate stays closed.
func TestPermitRuleMovedToUnknownPathFailsBoot(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Security.Policies = []web.PolicyRule{{Match: "GET /health", Permit: true}}

	server, ctx, _ := newPolicyBootServer(t, cfg)
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx), "the permit rule names the registered route")

	cfg.Security.Policies[0].Match = "GET /missing"
	server, ctx, host := newPolicyBootServer(t, cfg)
	require.NoError(t, server.Start(ctx))
	err := server.OpenTraffic(ctx)
	require.Error(t, err, "a mandatory permit rule that matches no route must fail the boot")
	assert.Contains(t, err.Error(), "policy 0")
	assert.Contains(t, err.Error(), `"GET /missing"`)
	assert.Contains(t, err.Error(), "optional: true")
	assert.False(t, host.trafficReleased(), "failed preparation must leave ingress blocked")
}
