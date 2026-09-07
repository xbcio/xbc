package casbin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// jwtKey mirrors the stable plugin.Key identity owned by the optional
// transport/web/extensions/authentication/jwt plugin. casbin must not import that sibling
// integration module (each optional integration is independently
// versioned), so the identity is duplicated here as a typed constant. The
// reference stays soft: any Principal-producing authentication plugin keeps
// Casbin usable when jwt is absent.
const jwtKey plugin.Key = "jwt"

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.authorize }

// Order implements web.Middleware.
func (p *Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseAuth,
		After: []web.OrderRef{web.Prefer(jwtKey)},
	}
}

func (p *Plugin) authorize(c *gin.Context) {
	route, found := web.CurrentRoute(c)
	if found && route.Auth.IsPublic() {
		c.Next()
		return
	}
	if !found {
		forbidden(c)
		return
	}

	if p.stopped.Load() {
		forbidden(c)
		return
	}
	state := p.state

	subject, ok := state.resolver.ResolveSubject(c)
	subject = strings.TrimSpace(subject)
	if !ok || subject == "" {
		forbidden(c)
		return
	}

	var request []any
	switch state.cfg.requestConvention {
	case ConventionRoutePermission:
		permission := strings.TrimSpace(route.Perm)
		if permission == "" {
			if state.cfg.missingPermission == MissingPermissionAllow {
				c.Next()
				return
			}
			forbidden(c)
			return
		}
		request = []any{subject, permission}
	case ConventionPathMethod:
		object := strings.TrimSpace(route.Path)
		action := strings.ToUpper(strings.TrimSpace(route.Method))
		if object == "" || action == "" {
			forbidden(c)
			return
		}
		request = []any{subject, object, action}
	default:
		forbidden(c)
		return
	}

	allowed, err := state.enforcer.Enforce(request...)
	if err != nil {
		state.logger.Error("casbin: authorization evaluation failed",
			"method", route.Method, "path", route.Path, "error", err)
		forbidden(c)
		return
	}
	if !allowed {
		forbidden(c)
		return
	}
	c.Next()
}

func forbidden(c *gin.Context) {
	web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
}
