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
// authorization/casbin plugin beneath the optional transport/web/extensions
// namespace. tenant is a transport/web built-in and must not import that
// extension module to preserve module boundaries, so the identity is
// duplicated here as a typed constant. The reference stays soft: a custom
// authorization stack remains valid when casbin is absent.
const casbinKey plugin.Key = "casbin"

// Handler returns the middleware handler.
func (p *Plugin) Handler() web.Handler { return p.resolve }

// Order places tenant resolution after the authentication middleware and
// before Casbin.
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
	gc := c.Gin()
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(gc) {
		c.Next()
		return nil
	}
	principal, ok := web.CurrentPrincipal(gc)
	if !ok {
		// Reachable in production: an unmatched route (404/405). The
		// authentication middleware passes those straight through without
		// marking them exempt or publishing a principal, and gin runs every
		// global middleware -- including this one -- on its NoRoute/NoMethod
		// path. There is no handler behind an unmatched path to protect, so
		// proceeding here is correct: it leaves the response to gin's
		// 404/405 instead of manufacturing a 403 for a route that does not
		// exist. This is a deliberate divergence from casbin, which turns
		// the same case into 403 via its own `!found` guard on CurrentRoute
		// (see the comment there).
		c.Next()
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
	if !setValidated(gc, resolved, state.cfg.minIDLength, state.cfg.maxIDLength) {
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

// forbidden takes *web.Ctx rather than the *gin.Context every other
// converted package's forbidden helper keeps, because resolve holds a
// *web.Ctx: a *gin.Context parameter would mean calling c.Gin() at all seven
// call sites. The nil check mirrors resolve's own top guard, which predates
// the engine-neutral rewrite. Both are unreachable through web.Handle today,
// since it dereferences the request before the handler runs; they stay as
// the guard rail for a direct caller.
func forbidden(c *web.Ctx) {
	if c == nil {
		return
	}
	web.AbortProblem(c.Gin(), web.NewProblem(http.StatusForbidden, "forbidden"))
}
