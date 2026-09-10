package tenant

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// casbinKey mirrors the stable plugin.Key identity owned by the optional
// authorization/casbin plugin beneath the optional transport/web/extensions
// namespace. tenant is a transport/web built-in and must not import that
// extension module to preserve module boundaries, so the identity is
// duplicated here as a typed constant. The reference stays soft: a custom
// authorization stack remains valid when casbin is absent.
const casbinKey plugin.Key = "casbin"

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return p.resolve }

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

func (p *Plugin) resolve(c *gin.Context) {
	if c == nil || c.Request == nil {
		forbidden(c)
		return
	}
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(c) {
		c.Next()
		return
	}
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		// A missing principal on a non-exempt route cannot happen in
		// production: the authentication middleware either publishes one
		// before tenant runs or rejects the request itself. Authentication is
		// no longer tenant's job to backstop, so this is a permit-route
		// characteristic here, not an error.
		c.Next()
		return
	}
	state := p.state.Load()
	if state == nil {
		forbidden(c)
		return
	}
	requested, present, valid := singleHeader(c, state.cfg.header)
	if !valid || (present && !validTenantID(requested, state.cfg.minIDLength, state.cfg.maxIDLength)) {
		forbidden(c)
		return
	}
	resolved, found, err := state.resolver.ResolveTenant(c.Request.Context(), principal, requested)
	if err != nil {
		forbidden(c)
		return
	}
	if !found {
		if state.cfg.required || present {
			forbidden(c)
			return
		}
		c.Next()
		return
	}
	if requested != "" && resolved.ID != requested {
		forbidden(c)
		return
	}
	if !setValidated(c, resolved, state.cfg.minIDLength, state.cfg.maxIDLength) {
		forbidden(c)
		return
	}
	c.Next()
}

func singleHeader(c *gin.Context, name string) (value string, present, valid bool) {
	if c == nil || c.Request == nil {
		return "", false, false
	}
	values := c.Request.Header.Values(name)
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) || strings.Contains(values[0], ",") {
		return "", true, false
	}
	return values[0], true, true
}

func forbidden(c *gin.Context) {
	if c != nil {
		web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
	}
}
