package web_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const routeAuthorizerWarning = "web: routes declare permissions but no middleware in the chain enforces Route.Perm"

const wantMissingRouteAuthorizerError = "xbc: 3 serving routes declare a permission but no middleware implements web.RouteAuthorizer\n  → Route.Perm is enforced only by a middleware that declares the contract; compose one (for example the casbin-http plugin), remove the permission declarations, or leave web.security.require_route_authorizer unset to keep this a warning"

const wantOneMissingRouteAuthorizerError = "xbc: 1 serving route declares a permission but no middleware implements web.RouteAuthorizer\n  → Route.Perm is enforced only by a middleware that declares the contract; compose one (for example the casbin-http plugin), remove the permission declarations, or leave web.security.require_route_authorizer unset to keep this a warning"

// authorizerMiddleware is an application middleware -- not a framework
// extension -- that declares the web.RouteAuthorizer contract. It is otherwise
// an ordinary contributed entry, ordered through the same chain as every other
// middleware, which is what the detection type-asserts.
type authorizerMiddleware struct{}

func (authorizerMiddleware) Handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		c.Next()
		return nil
	}
}

func (authorizerMiddleware) Order() web.Order {
	return web.Order{Phase: web.PhaseAuth, After: []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)}}
}

func (authorizerMiddleware) AuthorizesRoutes() {}

func middlewareEntry(key plugin.Key, value web.Middleware) plugin.Entry[web.Middleware] {
	return plugin.Entry[web.Middleware]{Identity: plugin.Identity{Plugin: key}, Value: value}
}

// permRouteContributor registers one GET route per permission, in declaration
// order, so the warning's sample can be observed against the catalog order.
func permRouteContributor(perms ...string) fakeRouteContributor {
	return fakeRouteContributor{register: func(router *web.Router) {
		for i, perm := range perms {
			path := fmt.Sprintf("/perm/%d", i)
			router.GET(path, func(context.Context, *web.Ctx) error { return nil }).Perm(perm)
		}
	}}
}

func captureHostLogger(host *fakeHost) *captureLogger {
	logger := &captureLogger{Logger: log.Nop()}
	host.logger = logger
	return logger
}

// TestPreflightWarnsOnceWhenPermissionsHaveNoAuthorizer pins the validate half
// of the visibility change: a route carrying .Perm with no middleware declaring
// web.RouteAuthorizer is still accepted, but the one warning names the count
// and at most three routes. A plain middleware in the chain must not count as
// an authorizer.
func TestPreflightWarnsOnceWhenPermissionsHaveNoAuthorizer(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	plain := fakeMiddleware{
		handler: func(_ context.Context, c *web.Ctx) error { c.Next(); return nil },
		order:   web.Order{Phase: web.PhaseAuth, After: []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)}},
	}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{middlewareEntry("plain-app", plain)},
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read", "orders:write", "orders:delete", "orders:admin"),
		}},
	})
	logger := captureHostLogger(host)

	require.NoError(t, server.Preflight(ctx),
		"a missing authorizer is a warning by default, not a validation failure")

	warns := logger.warns()
	require.Len(t, warns, 1, "the warning is emitted once per freeze")
	assert.Contains(t, warns[0], routeAuthorizerWarning)
	assert.Contains(t, warns[0], "routes=4")
	assert.Contains(t, warns[0], "sample=[GET /perm/0 GET /perm/1 GET /perm/2]",
		"the sample is capped at three routes, in catalog order")
	assert.Contains(t, warns[0], "require_route_authorizer=false")
}

// TestOpenTrafficWarnsOnceWhenPermissionsHaveNoAuthorizer pins the boot half:
// the same warning reaches a real start's preparation without failing it.
func TestOpenTrafficWarnsOnceWhenPermissionsHaveNoAuthorizer(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read", "orders:write", "orders:delete"),
		}},
	})
	logger := captureHostLogger(host)

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx), "the default keeps the composition booting")

	warns := logger.warns()
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], routeAuthorizerWarning)
	assert.Contains(t, warns[0], "routes=3")
	assert.Contains(t, warns[0], "sample=[GET /perm/0 GET /perm/1 GET /perm/2]")
	assert.Contains(t, warns[0], "require_route_authorizer=false")
}

// TestRequireRouteAuthorizerFailsPreflightWithoutNotifyingListeners pins the
// opt-in failure on the validate path and, more importantly, that it happens
// before the listener loop: a listener must not observe a route table whose
// permissions nothing enforces.
func TestRequireRouteAuthorizerFailsPreflightWithoutNotifyingListeners(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Security.RequireRouteAuthorizer = true
	listenerCalled := false
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read", "orders:write", "orders:delete"),
		}},
		listeners: []plugin.Entry[web.RouteCatalogListener]{{
			Identity: plugin.Identity{Plugin: "catalog"},
			Value: fakeRouteCatalogListener{ready: func(web.RouteCatalog) error {
				listenerCalled = true
				return nil
			}},
		}},
	})
	logger := captureHostLogger(host)

	err := server.Preflight(ctx)
	require.EqualError(t, err, wantMissingRouteAuthorizerError)
	assert.False(t, listenerCalled, "the check must precede listener notification")
	assert.Empty(t, logger.warns(), "the require path fails instead of warning")

	assert.Empty(t, server.Addr(), "the failure must be found before anything binds")
	assert.Empty(t, host.submittedTasks(), "Preflight must admit no serving task")
}

// TestRequireRouteAuthorizerErrorUsesSingularForOneRoute pins the one-route
// wording. The count is the actionable part of the diagnostic, so a single
// route must not read as "1 serving routes declare".
func TestRequireRouteAuthorizerErrorUsesSingularForOneRoute(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Security.RequireRouteAuthorizer = true
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read"),
		}},
	})

	err := server.Preflight(ctx)
	require.EqualError(t, err, wantOneMissingRouteAuthorizerError)
}

// TestRequireRouteAuthorizerFailsOpenTrafficWithoutNotifyingListeners is the
// boot-side counterpart: Start succeeds, preparation fails with the same exact
// error, and no listener has seen the table.
func TestRequireRouteAuthorizerFailsOpenTrafficWithoutNotifyingListeners(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Security.RequireRouteAuthorizer = true
	listenerCalled := false
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read", "orders:write", "orders:delete"),
		}},
		listeners: []plugin.Entry[web.RouteCatalogListener]{{
			Identity: plugin.Identity{Plugin: "catalog"},
			Value: fakeRouteCatalogListener{ready: func(web.RouteCatalog) error {
				listenerCalled = true
				return nil
			}},
		}},
	})
	logger := captureHostLogger(host)

	require.NoError(t, server.Start(ctx))
	err := server.OpenTraffic(ctx)
	require.EqualError(t, err, wantMissingRouteAuthorizerError)
	assert.False(t, listenerCalled, "the check must precede listener notification")
	assert.False(t, host.trafficReleased(), "failed preparation must leave ingress blocked")
	assert.Empty(t, logger.warns(), "the require path fails instead of warning")
}

// TestContributedRouteAuthorizerSuppressesTheWarning pins the positive side:
// an application middleware implementing the marker is detected by the type
// assertion over the ordered chain, even beside a plain middleware and in a
// later phase than authentication.
func TestContributedRouteAuthorizerSuppressesTheWarning(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	plain := fakeMiddleware{
		handler: func(_ context.Context, c *web.Ctx) error { c.Next(); return nil },
		order:   web.Order{Phase: web.PhaseBusiness},
	}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{
			middlewareEntry("plain-app", plain),
			middlewareEntry("app-authorizer", authorizerMiddleware{}),
		},
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value:    permRouteContributor("orders:read", "orders:write"),
		}},
	})
	logger := captureHostLogger(host)

	require.NoError(t, server.Preflight(ctx))
	assert.Empty(t, logger.warns(),
		"a contributed middleware declaring RouteAuthorizer must suppress the warning")
}

// TestPermissionCheckRunsAfterAuthenticationPolicy pins the check's position
// in freezeRoutes. The authentication policy accepts or rejects the frozen
// table before permissions are examined, so a table it rejects must fail
// without the authorizer warning: the policy error is the actionable one, and a
// warning emitted first would describe a composition the server never reached.
func TestPermissionCheckRunsAfterAuthenticationPolicy(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "orders"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/private", func(context.Context, *web.Ctx) error { return nil }).
					Auth(web.Accepts("jwt")).
					Perm("orders:read")
			}},
		}},
	})
	logger := captureHostLogger(host)

	err := server.Preflight(ctx)
	require.Error(t, err, "a route requiring authentication with no registered authenticator must fail")
	assert.Contains(t, err.Error(), "no authenticator is registered")
	assert.Empty(t, logger.warns(),
		"the authorizer check must run only after the authentication policy accepts the table")
}

// TestNoPermissionDeclarationsProduceNoWarning pins the silent case: without a
// single serving route carrying a .Perm, the chain's composition is irrelevant.
func TestNoPermissionDeclarationsProduceNoWarning(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	logger := captureHostLogger(host)

	require.NoError(t, server.Preflight(ctx))
	assert.Empty(t, logger.warns(), "no permission declaration means nothing to warn about")
}
