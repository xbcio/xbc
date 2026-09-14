package casbin

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.authorize) }

// Order implements web.Middleware.
func (p *Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseAuth,
		After: []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)},
	}
}

func (p *Plugin) authorize(_ context.Context, c *web.Ctx) error {
	gc := c.Gin()
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(gc) {
		c.Next()
		return nil
	}
	route, found := web.CurrentRoute(gc)
	if !found {
		// This is a deliberate divergence from tenant, which passes an
		// unmatched route (404/405) straight through to preserve gin's own
		// response (see the comment on tenant's equivalent branch). Rewriting
		// it to 403 here is pre-existing behavior, not introduced by the
		// exemption-model change: flipping it would alter the observable
		// response for every unmatched path in every casbin application, and
		// fail-closed is the safer side to leave standing.
		forbidden(gc)
		return nil
	}

	if p.stopped.Load() {
		forbidden(gc)
		return nil
	}
	state := p.state

	subject, ok := state.resolver.ResolveSubject(gc)
	subject = strings.TrimSpace(subject)
	if !ok || subject == "" {
		forbidden(gc)
		return nil
	}

	var request []any
	switch state.cfg.requestConvention {
	case ConventionRoutePermission:
		permission := strings.TrimSpace(route.Perm)
		if permission == "" {
			if state.cfg.missingPermission == MissingPermissionAllow {
				c.Next()
				return nil
			}
			forbidden(gc)
			return nil
		}
		request = []any{subject, permission}
	case ConventionPathMethod:
		object := strings.TrimSpace(route.Path)
		action := strings.ToUpper(strings.TrimSpace(route.Method))
		if object == "" || action == "" {
			forbidden(gc)
			return nil
		}
		request = []any{subject, object, action}
	default:
		forbidden(gc)
		return nil
	}

	allowed, err := state.enforcer.Enforce(request...)
	if err != nil {
		state.logger.Error("casbin: authorization evaluation failed",
			"method", route.Method, "path", route.Path, "error", err)
		forbidden(gc)
		return nil
	}
	if !allowed {
		forbidden(gc)
		return nil
	}
	c.Next()
	return nil
}

func forbidden(c *gin.Context) {
	web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
}
