package tenant

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
)

// jwtKey, sessionKey, and casbinKey mirror stable plugin.Key identities owned
// by authentication/{jwt,session} and authorization/casbin beneath the optional
// transport/web/extensions namespace. tenant is a transport/web built-in and
// must not import those extension modules to preserve module boundaries, so
// the identities are duplicated here as typed constants. Every reference stays
// soft: custom auth/authorization stacks remain valid when any named plugin
// is absent.
const (
	jwtKey     plugin.Key = "jwt"
	sessionKey plugin.Key = "session"
	casbinKey  plugin.Key = "casbin"
)

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return p.resolve }

// Order places tenant resolution after common authentication plugins and
// before Casbin.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseAuth,
		After: []web.OrderRef{
			web.Prefer(jwtKey),
			web.Prefer(apikey.Key),
			web.Prefer(sessionKey),
		},
		Before: []web.OrderRef{web.Prefer(casbinKey)},
	}
}

func (p *Plugin) resolve(c *gin.Context) {
	if route, ok := web.CurrentRoute(c); ok && route.Auth.IsPublic() {
		c.Next()
		return
	}
	if c == nil || c.Request == nil {
		forbidden(c)
		return
	}
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		unauthorized(c)
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

func unauthorized(c *gin.Context) {
	if c != nil {
		web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
	}
}

func forbidden(c *gin.Context) {
	if c != nil {
		web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
	}
}
