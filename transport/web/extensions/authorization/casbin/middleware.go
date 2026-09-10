package casbin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.authorize }

// Order implements web.Middleware.
func (p *Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseAuth,
		After: []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)},
	}
}

func (p *Plugin) authorize(c *gin.Context) {
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(c) {
		c.Next()
		return
	}
	route, found := web.CurrentRoute(c)
	if !found {
		// This is a deliberate divergence from tenant, which passes an
		// unmatched route (404/405) straight through to preserve gin's own
		// response (see the comment on tenant's equivalent branch). Rewriting
		// it to 403 here is pre-existing behavior, not introduced by the
		// exemption-model change: flipping it would alter the observable
		// response for every unmatched path in every casbin application, and
		// fail-closed is the safer side to leave standing. Ruling 23 leaves
		// both extensions as they are.
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
