package web_test

import (
	"context"
	"net"
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

// TestPreflightValidatesRoutePolicyWithoutStartingOrBinding pins the point of
// the Preflight stage: it runs the route-policy validation OpenTraffic runs --
// the one that otherwise first reports a mistake after Start has already
// assembled and bound -- while starting nothing and binding nothing.
//
// The composition is deliberately the one
// TestOpenTrafficFailsWhenARouteFallsToDenyWithoutAuthenticator uses, so the two
// failures are the same failure reached from two different commands: there,
// Start had already run, and here it never does. Preflight is also expected to
// leave the Server unstarted, which OpenTraffic then confirms by refusing to run
// at all.
func TestPreflightValidatesRoutePolicyWithoutStartingOrBinding(t *testing.T) {
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

	err := server.Preflight(ctx)
	require.Error(t, err, "an endpoint without an auth policy must not validate without an authenticator")
	assert.ErrorContains(t, err, "GET /-/metrics")
	assert.ErrorContains(t, err, "requires authentication but no authenticator is registered")

	assert.Empty(t, server.Addr(), "Preflight must not bind the configured address")
	assert.Empty(t, host.submittedTasks(), "Preflight must admit no serving task")
	assert.False(t, host.trafficReleased(), "Preflight must not release the traffic gate")
	assert.ErrorContains(t, server.OpenTraffic(ctx), "has not started successfully",
		"Preflight assembles for the report; it must leave the Server itself unstarted")
}

// TestPreflightRejectsAnUnknownAuthenticationScheme pins the other policy
// mistake the stage exists to catch before a process is deployed: a route
// selection naming a scheme no registered authenticator provides. It is only
// visible once the authenticators exist and the route table is frozen, which is
// exactly the moment Preflight runs and the moment a start would otherwise have
// already bound its listener.
func TestPreflightRejectsAnUnknownAuthenticationScheme(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Security.Policies = []web.PolicyRule{{
		Match:        "/api/**",
		Authenticate: []authentication.Scheme{"nope"},
	}}

	orders := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/api/v1/orders", func(context.Context, *web.Ctx) error { return nil }).Name("orders.list")
	}}
	host := newFakeHost()
	ctx := contextFromHost(host)
	// Built without newPingServer on purpose: that helper permits by default,
	// which turns on the registered-but-unreferenced-scheme check and reports
	// that instead. The route selection is the mistake under test here.
	server := web.NewServer(
		cfg,
		enginetest.Factory{},
		nil,
		nil,
		[]plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orderstest"},
			Value:    orders,
		}},
		nil,
		[]plugin.Entry[authentication.Authenticator]{{
			Identity: plugin.Identity{Plugin: "apikey"},
			Value:    &stubAuth{scheme: "apikey"},
		}},
		nil,
	)
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	err := server.Preflight(ctx)
	require.Error(t, err, "a route selection naming an unregistered scheme must fail validation")
	assert.ErrorContains(t, err, `unknown scheme "nope"`)
	assert.ErrorContains(t, err, "GET /api/v1/orders",
		"the failure must name the route that would have been served under the mistake")

	assert.Empty(t, server.Addr())
	assert.Empty(t, host.submittedTasks())
}

// TestPreflightPrintsTheStartupReportAndBindsNothing pins the other half: a
// composition that validates prints the report a start prints, and does it with
// a port it cannot have taken. The configured address is already held by this
// test, so a Preflight that bound it would fail with an address-in-use error
// instead of returning the report -- which is stronger evidence than reading an
// empty Addr() back, since it fails on the attempt rather than on the record.
func TestPreflightPrintsTheStartupReportAndBindsNothing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	cfg := web.DefaultConfig()
	cfg.Addr = held.Addr().String()
	cfg.Security.Default = web.SecurityPermit

	logger := &captureLogger{Logger: log.Nop()}
	host := newFakeHost()
	host.logger = logger
	ctx := contextFromHost(host)

	ping := fakeRouteContributor{register: func(router *web.Router) {
		router.GET("/ping", func(context.Context, *web.Ctx) error { return nil }).Name("pingtest.ping")
	}}
	extra := fakeRouteContributor{register: func(router *web.Router) {
		router.POST("/extra", func(context.Context, *web.Ctx) error { return nil }).Name("preflight.extra")
	}}
	var catalog web.RouteCatalog
	listener := fakeRouteCatalogListener{ready: func(received web.RouteCatalog) error {
		catalog = received
		return nil
	}}

	server := web.NewServer(
		cfg,
		enginetest.Factory{},
		nil,
		nil,
		[]plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "pingtest"}, Value: ping},
			{Identity: plugin.Identity{Plugin: "extratest"}, Value: extra},
		},
		[]plugin.Entry[web.RouteCatalogListener]{{
			Identity: plugin.Identity{Plugin: "catalogtest"},
			Value:    listener,
		}},
		nil, nil,
	)
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	require.NoError(t, server.Preflight(ctx),
		"a valid composition must validate against an address another process holds")

	require.NotNil(t, catalog, "Preflight must freeze the route table and notify listeners, as OpenTraffic does")
	registered := make([]string, 0, len(catalog.All()))
	for _, route := range catalog.All() {
		registered = append(registered, route.Method+" "+route.Path)
	}
	assert.Contains(t, registered, "GET /ping")
	assert.Contains(t, registered, "POST /extra",
		"every contributor registers against the same table Preflight freezes")

	report := strings.Join(logger.infos(), "\n")
	assert.Contains(t, report, "web: route table (2)")
	assert.Contains(t, report, "web: policy decisions (2)")

	assert.Empty(t, server.Addr(), "Preflight must not bind the configured address")
	assert.Empty(t, host.submittedTasks(), "Preflight must admit no serving task")
	assert.False(t, host.trafficReleased(), "Preflight must not release the traffic gate")

	// The report is not the whole contract: Preflight must leave the Server
	// exactly as unstarted as it found it. A Preflight that stored the pipeline
	// the way Start does would make this call succeed -- OpenTraffic would skip
	// the freeze and the report it is supposed to perform and serve whatever
	// table it found.
	err = server.OpenTraffic(ctx)
	require.Error(t, err, "a preflighted Server must still be unstarted")
	assert.Contains(t, err.Error(), "has not started successfully")
}

// TestAPreflightLeavesNothingBehindThatChangesALaterBoot pins the other half of
// "activates nothing": what Preflight assembles is local to the call. A
// Preflight that stored its pipeline the way a boot does would not fail any
// single-call assertion -- it would surface here, by making the OpenTraffic that
// follows a real Start return early, skipping the freeze and the report it owes.
// Counting the reports is the observable, because the two calls render the same
// report by design and only the second one's existence is in question.
func TestAPreflightLeavesNothingBehindThatChangesALaterBoot(t *testing.T) {
	logger := &captureLogger{Logger: log.Nop()}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	host.logger = logger
	t.Cleanup(func() {
		assert.NoError(t, server.Stop(context.Background()))
		host.shutdown()
	})

	require.NoError(t, server.Preflight(ctx), "the composition must validate before it boots")
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))

	assert.Equal(t, 2, strings.Count(strings.Join(logger.infos(), "\n"), "web: route table (1)"),
		"Preflight and OpenTraffic each freeze and report a route table of their own")
}
