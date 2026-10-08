package tenant

import (
	"context"
	"net/http"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Plugin declares authentication.RequiresPrincipal at compile time. tenant
// does not otherwise import the authentication package, so it satisfies the
// interface structurally; without this assertion, an upstream rename of the
// marker method would silently unbind it and this package would still build.
var _ authentication.RequiresPrincipal = (*Plugin)(nil)

// casbinKey mirrors the stable plugin.Key identity owned by the optional
// route-authorization middleware beneath the optional
// transport/web/extensions namespace. That middleware is keyed "casbin-http"
// since the policy engine it enforces became a protocol-neutral plugin of its
// own under the key "casbin"; this constant names the Web middleware, which is
// the only one of the two that occupies a place in the middleware order.
// tenant is a transport/web built-in and must not import that extension module
// to preserve module boundaries, so the identity is duplicated here as a typed
// constant. The reference stays soft: a custom authorization stack remains
// valid when casbin-http is absent.
const casbinKey plugin.Key = "casbin-http"

// Handler returns the middleware handler.
func (p *Plugin) Handler() web.Handler { return p.resolve }

// Order places tenant resolution after the authentication middleware and
// before the Casbin route-authorization middleware.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase:  web.PhaseAuth,
		After:  []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)},
		Before: []web.OrderRef{web.Prefer(casbinKey)},
	}
}

// RequiresPrincipal declares that this middleware cannot run before an
// identity is published, which makes the framework pin it after the
// authentication middleware automatically. Order already declares the same
// edge explicitly; this marker documents the dependency at the type level too.
func (*Plugin) RequiresPrincipal() {}

func (p *Plugin) resolve(_ context.Context, c *web.Ctx) error {
	if c == nil || c.Request() == nil {
		forbidden(c)
		return nil
	}
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(c) {
		c.Next()
		return nil
	}
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		// A real route (CurrentRoute hits) but no principal can now mean two
		// things: an unmatched route (404/405), or a request the authentication
		// middleware let through via AcceptedWithoutPrincipal (trusted but not a
		// natural person -- a gateway signature, a service-to-service call).
		// Being authenticated and being exempt from a subject requirement are
		// different facts: an unmatched route is still passed through, since
		// there is no handler behind it to protect and manufacturing a 403 for a
		// nonexistent route would be wrong. A real route with no subject cannot
		// resolve a tenant, so it is refused -- this is the only path this task
		// adds, and it closes what would otherwise be a tenant-isolation bypass
		// for an authenticated-without-principal caller. The web.CurrentRoute
		// check mirrors casbin's own `!found` guard (see the comment there), so
		// the two extensions no longer diverge on this branch.
		if _, found := web.CurrentRoute(c); !found {
			c.Next()
			return nil
		}
		forbidden(c)
		return nil
	}
	state := p.state.Load()
	if state == nil {
		forbidden(c)
		return nil
	}
	requested, present, valid := singleHeader(c, state.cfg.header)
	if !valid || (present && !validTenantID(requested, state.cfg.minIDLength, state.cfg.maxIDLength)) {
		forbidden(c)
		return nil
	}
	resolved, found, err := state.resolver.ResolveTenant(c.Request().Context(), principal, requested)
	if err != nil {
		forbidden(c)
		return nil
	}
	if !found {
		if state.cfg.required || present {
			forbidden(c)
			return nil
		}
		c.Next()
		return nil
	}
	if requested != "" && resolved.ID != requested {
		forbidden(c)
		return nil
	}
	if !setValidated(c, resolved, state.cfg.minIDLength, state.cfg.maxIDLength) {
		forbidden(c)
		return nil
	}
	c.Next()
	return nil
}

func singleHeader(c *web.Ctx, name string) (value string, present, valid bool) {
	if c == nil || c.Request() == nil {
		return "", false, false
	}
	values := c.Request().Header.Values(name)
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) || strings.Contains(values[0], ",") {
		return "", true, false
	}
	return values[0], true, true
}

// forbidden takes *web.Ctx, matching web.AbortProblem's own neutral signature:
// no call site needs to reach for c.Gin() any more. The nil check mirrors
// resolve's own top guard, which predates the engine-neutral rewrite. Both are
// unreachable through web.Handle today, since it dereferences the request
// before the handler runs; they stay as the guard rail for a direct caller.
func forbidden(c *web.Ctx) {
	if c == nil {
		return
	}
	web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
}
